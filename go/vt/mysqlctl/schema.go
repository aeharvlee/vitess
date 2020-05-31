/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysqlctl

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"vitess.io/vitess/go/vt/vtgate/evalengine"

	"golang.org/x/net/context"

	"vitess.io/vitess/go/sqlescape"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"

	querypb "vitess.io/vitess/go/vt/proto/query"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
)

var autoIncr = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

// executeSchemaCommands executes some SQL commands, using the mysql
// command line tool. It uses the dba connection parameters, with credentials.
func (mysqld *Mysqld) executeSchemaCommands(sql string) error {
	params, err := mysqld.dbcfgs.DbaConnector().MysqlParams()
	if err != nil {
		return err
	}

	return mysqld.executeMysqlScript(params, strings.NewReader(sql))
}

// tableList returns an IN clause "('t1', 't2'...) for a list of tables."
func tableListSql(tables []string) string {
	if len(tables) == 0 {
		return "()"
	}

	return "('" + strings.Join(tables, "', '") + "')"
}

// GetSchema returns the schema for database for tables listed in
// tables. If tables is empty, return the schema for all tables.
func (mysqld *Mysqld) GetSchema(ctx context.Context, dbName string, tables, excludeTables []string, includeViews bool) (*tabletmanagerdatapb.SchemaDefinition, error) {
	sd := &tabletmanagerdatapb.SchemaDefinition{}
	backtickDBName := sqlescape.EscapeID(dbName)

	// get the database creation command
	qr, fetchErr := mysqld.FetchSuperQuery(ctx, fmt.Sprintf("SHOW CREATE DATABASE IF NOT EXISTS %s", backtickDBName))
	if fetchErr != nil {
		return nil, fetchErr
	}
	if len(qr.Rows) == 0 {
		return nil, fmt.Errorf("empty create database statement for %v", dbName)
	}
	sd.DatabaseSchema = strings.Replace(qr.Rows[0][1].ToString(), backtickDBName, "{{.DatabaseName}}", 1)

	// get the list of tables we're interested in
	sql := "SELECT table_name, table_type, data_length, table_rows FROM information_schema.tables WHERE table_schema = '" + dbName + "'"
	if !includeViews {
		sql += " AND table_type = '" + tmutils.TableBaseTable + "'"
	}
	qr, err := mysqld.FetchSuperQuery(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(qr.Rows) == 0 {
		return sd, nil
	}

	filter, err := tmutils.NewTableFilter(tables, excludeTables, includeViews)
	if err != nil {
		return nil, err
	}

	sd.TableDefinitions = make([]*tabletmanagerdatapb.TableDefinition, 0, len(qr.Rows))
	tdMap := map[string]*tabletmanagerdatapb.TableDefinition{}
	for _, row := range qr.Rows {
		tableName := row[0].ToString()
		tableType := row[1].ToString()

		if !filter.Includes(tableName, tableType) {
			continue
		}

		// compute dataLength
		var dataLength uint64
		if !row[2].IsNull() {
			// dataLength is NULL for views, then we use 0
			dataLength, err = evalengine.ToUint64(row[2])
			if err != nil {
				return nil, err
			}
		}

		// get row count
		var rowCount uint64
		if !row[3].IsNull() {
			rowCount, err = evalengine.ToUint64(row[3])
			if err != nil {
				return nil, err
			}
		}

		td := &tabletmanagerdatapb.TableDefinition{
			Name:       tableName,
			Type:       tableType,
			DataLength: dataLength,
			RowCount:   rowCount,
		}
		sd.TableDefinitions = append(sd.TableDefinitions, td)
		tdMap[tableName] = td
	}

	log.Infof("mysqld GetSchema: GetPrimaryKeyColumns")
	tableNames := make([]string, 0, len(tdMap))
	for tableName := range tdMap {
		tableNames = append(tableNames, tableName)
	}
	colMap, err := mysqld.getPrimaryKeyColumns(ctx, dbName, tableNames...)
	if err != nil {
		return nil, err
	}
	log.Infof("mysqld GetSchema: GetPrimaryKeyColumns done")
	for tableName, td := range tdMap {
		td.PrimaryKeyColumns = colMap[tableName]
	}

	log.Infof("mysqld GetSchema: Collecting all table schemas")
	resChan := make(chan *schemaResult, 100)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for tableName := range tdMap {
		wg.Add(1)
		go func(tableName, tableType string) {
			res := mysqld.collectSchema(ctx, dbName, tableName, tableType)
			resChan <- res
		}(tableName, tdMap[tableName].Type)
	}
	wg.Wait()
	close(resChan)

	for res := range resChan {
		if res.err != nil {
			cancel()
			return nil, res.err
		}

		td := tdMap[res.tableName]
		td.Fields = res.fields
		td.Columns = res.columns
		td.Schema = res.schema
	}
	log.Infof("mysqld GetSchema: Collecting all table schemas done")

	tmutils.GenerateSchemaVersion(sd)
	return sd, nil
}

type schemaResult struct {
	tableName string
	fields    []*querypb.Field
	columns   []string
	schema    string
	err       error
}

func (mysqld *Mysqld) collectSchema(ctx context.Context, dbName, tableName, tableType string) *schemaResult {
	fields, columns, err := mysqld.GetColumns(ctx, dbName, tableName)
	if err != nil {
		return &schemaResult{
			tableName: tableName,
			err:       err,
		}
	}

	schema, err := mysqld.normalizedSchema(ctx, dbName, tableName, tableType)
	if err != nil {
		return &schemaResult{
			tableName: tableName,
			err:       err,
		}
	}

	return &schemaResult{
		tableName: tableName,
		fields:    fields,
		columns:   columns,
		schema:    schema,
		err:       err,
	}
}

func (mysqld *Mysqld) normalizedSchema(ctx context.Context, dbName, tableName, tableType string) (string, error) {
	backtickDBName := sqlescape.EscapeID(dbName)
	qr, fetchErr := mysqld.FetchSuperQuery(ctx, fmt.Sprintf("SHOW CREATE TABLE %s.%s", dbName, sqlescape.EscapeID(tableName)))
	if fetchErr != nil {
		return "", fetchErr
	}
	if len(qr.Rows) == 0 {
		return "", fmt.Errorf("empty create table statement for %v", tableName)
	}

	// Normalize & remove auto_increment because it changes on every insert
	// FIXME(alainjobart) find a way to share this with
	// vt/tabletserver/table_info.go:162
	norm := qr.Rows[0][1].ToString()
	norm = autoIncr.ReplaceAllLiteralString(norm, "")
	if tableType == tmutils.TableView {
		// Views will have the dbname in there, replace it
		// with {{.DatabaseName}}
		norm = strings.Replace(norm, backtickDBName, "{{.DatabaseName}}", -1)
	}

	return norm, nil
}

// ResolveTables returns a list of actual tables+views matching a list
// of regexps
func ResolveTables(ctx context.Context, mysqld MysqlDaemon, dbName string, tables []string) ([]string, error) {
	sd, err := mysqld.GetSchema(ctx, dbName, tables, nil, true)
	if err != nil {
		return nil, err
	}
	result := make([]string, len(sd.TableDefinitions))
	for i, td := range sd.TableDefinitions {
		result[i] = td.Name
	}
	return result, nil
}

// GetColumns returns the columns of table.
func (mysqld *Mysqld) GetColumns(ctx context.Context, dbName, table string) ([]*querypb.Field, []string, error) {
	conn, err := getPoolReconnect(ctx, mysqld.dbaPool)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Recycle()

	qr, err := conn.ExecuteFetch(fmt.Sprintf("SELECT * FROM %s.%s WHERE 1=0", sqlescape.EscapeID(dbName), sqlescape.EscapeID(table)), 0, true)
	if err != nil {
		return nil, nil, err
	}

	columns := make([]string, len(qr.Fields))
	for i, field := range qr.Fields {
		columns[i] = field.Name
	}
	return qr.Fields, columns, nil

}

// GetPrimaryKeyColumns returns the primary key columns of table.
func (mysqld *Mysqld) GetPrimaryKeyColumns(ctx context.Context, dbName, table string) ([]string, error) {
	cs, err := mysqld.getPrimaryKeyColumns(ctx, dbName, table)
	if err != nil {
		return nil, err
	}

	return cs[dbName], nil
}

func (mysqld *Mysqld) getPrimaryKeyColumns(ctx context.Context, dbName string, tables ...string) (map[string][]string, error) {
	conn, err := getPoolReconnect(ctx, mysqld.dbaPool)
	if err != nil {
		return nil, err
	}
	defer conn.Recycle()

	tableList := tableListSql(tables)
	sql := fmt.Sprintf(`
		SELECT table_name, ordinal_position, column_name
		FROM information_schema.key_column_usage
		WHERE table_schema = '%s'
			AND table_name IN %s
			AND constraint_name='PRIMARY'
		ORDER BY table_name, ordinal_position`, dbName, tableList)
	qr, err := conn.ExecuteFetch(sql, len(tables)*100, true)
	if err != nil {
		return nil, err
	}

	colMap := map[string][]string{}
	for _, row := range qr.Rows {
		tableName := row[0].ToString()

		columns, ok := colMap[tableName]
		if !ok {
			columns = make([]string, 0, 5)
			colMap[tableName] = columns
		}

		columns = append(columns, row[2].ToString())
	}
	return colMap, err
}

// PreflightSchemaChange checks the schema changes in "changes" by applying them
// to an intermediate database that has the same schema as the target database.
func (mysqld *Mysqld) PreflightSchemaChange(ctx context.Context, dbName string, changes []string) ([]*tabletmanagerdatapb.SchemaChangeResult, error) {
	results := make([]*tabletmanagerdatapb.SchemaChangeResult, len(changes))

	// Get current schema from the real database.
	originalSchema, err := mysqld.GetSchema(ctx, dbName, nil, nil, true)
	if err != nil {
		return nil, err
	}

	// Populate temporary database with it.
	initialCopySQL := "SET sql_log_bin = 0;\n"
	initialCopySQL += "DROP DATABASE IF EXISTS _vt_preflight;\n"
	initialCopySQL += "CREATE DATABASE _vt_preflight;\n"
	initialCopySQL += "USE _vt_preflight;\n"
	// We're not smart enough to create the tables in a foreign-key-compatible way,
	// so we temporarily disable foreign key checks while adding the existing tables.
	initialCopySQL += "SET foreign_key_checks = 0;\n"
	for _, td := range originalSchema.TableDefinitions {
		if td.Type == tmutils.TableBaseTable {
			initialCopySQL += td.Schema + ";\n"
		}
	}
	for _, td := range originalSchema.TableDefinitions {
		if td.Type == tmutils.TableView {
			// Views will have {{.DatabaseName}} in there, replace
			// it with _vt_preflight
			s := strings.Replace(td.Schema, "{{.DatabaseName}}", "`_vt_preflight`", -1)
			initialCopySQL += s + ";\n"
		}
	}
	if err = mysqld.executeSchemaCommands(initialCopySQL); err != nil {
		return nil, err
	}

	// For each change, record the schema before and after.
	for i, change := range changes {
		beforeSchema, err := mysqld.GetSchema(ctx, "_vt_preflight", nil, nil, true)
		if err != nil {
			return nil, err
		}

		// apply schema change to the temporary database
		sql := "SET sql_log_bin = 0;\n"
		sql += "USE _vt_preflight;\n"
		sql += change
		if err = mysqld.executeSchemaCommands(sql); err != nil {
			return nil, err
		}

		// get the result
		afterSchema, err := mysqld.GetSchema(ctx, "_vt_preflight", nil, nil, true)
		if err != nil {
			return nil, err
		}

		results[i] = &tabletmanagerdatapb.SchemaChangeResult{BeforeSchema: beforeSchema, AfterSchema: afterSchema}
	}

	// and clean up the extra database
	dropSQL := "SET sql_log_bin = 0;\n"
	dropSQL += "DROP DATABASE _vt_preflight;\n"
	if err = mysqld.executeSchemaCommands(dropSQL); err != nil {
		return nil, err
	}

	return results, nil
}

// ApplySchemaChange will apply the schema change to the given database.
func (mysqld *Mysqld) ApplySchemaChange(ctx context.Context, dbName string, change *tmutils.SchemaChange) (*tabletmanagerdatapb.SchemaChangeResult, error) {
	// check current schema matches
	beforeSchema, err := mysqld.GetSchema(ctx, dbName, nil, nil, true)
	if err != nil {
		return nil, err
	}
	if change.BeforeSchema != nil {
		schemaDiffs := tmutils.DiffSchemaToArray("actual", beforeSchema, "expected", change.BeforeSchema)
		if len(schemaDiffs) > 0 {
			for _, msg := range schemaDiffs {
				log.Warningf("BeforeSchema differs: %v", msg)
			}

			// let's see if the schema was already applied
			if change.AfterSchema != nil {
				schemaDiffs = tmutils.DiffSchemaToArray("actual", beforeSchema, "expected", change.AfterSchema)
				if len(schemaDiffs) == 0 {
					// no diff between the schema we expect
					// after the change and the current
					// schema, we already applied it
					return &tabletmanagerdatapb.SchemaChangeResult{
						BeforeSchema: beforeSchema,
						AfterSchema:  beforeSchema}, nil
				}
			}

			if change.Force {
				log.Warningf("BeforeSchema differs, applying anyway")
			} else {
				return nil, fmt.Errorf("BeforeSchema differs")
			}
		}
	}

	sql := change.SQL
	if !change.AllowReplication {
		sql = "SET sql_log_bin = 0;\n" + sql
	}

	// add a 'use XXX' in front of the SQL
	sql = fmt.Sprintf("USE %s;\n%s", sqlescape.EscapeID(dbName), sql)

	// execute the schema change using an external mysql process
	// (to benefit from the extra commands in mysql cli)
	if err = mysqld.executeSchemaCommands(sql); err != nil {
		return nil, err
	}

	// get AfterSchema
	afterSchema, err := mysqld.GetSchema(ctx, dbName, nil, nil, true)
	if err != nil {
		return nil, err
	}

	// compare to the provided AfterSchema
	if change.AfterSchema != nil {
		schemaDiffs := tmutils.DiffSchemaToArray("actual", afterSchema, "expected", change.AfterSchema)
		if len(schemaDiffs) > 0 {
			for _, msg := range schemaDiffs {
				log.Warningf("AfterSchema differs: %v", msg)
			}
			if change.Force {
				log.Warningf("AfterSchema differs, not reporting error")
			} else {
				return nil, fmt.Errorf("AfterSchema differs")
			}
		}
	}

	return &tabletmanagerdatapb.SchemaChangeResult{BeforeSchema: beforeSchema, AfterSchema: afterSchema}, nil
}
