package bigquery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/utils/googleutils"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/warehouse/client"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

var (
	pkgLogger                             logger.Logger
	setUsersLoadPartitionFirstEventFilter bool
	customPartitionsEnabled               bool
	isUsersTableDedupEnabled              bool
	isDedupEnabled                        bool
	enableDeleteByJobs                    bool
)

type HandleT struct {
	backgroundContext context.Context
	db                *bigquery.Client
	namespace         string
	warehouse         warehouseutils.Warehouse
	projectID         string
	uploader          warehouseutils.UploaderI
}

type StagingLoadTableT struct {
	partitionDate    string
	stagingTableName string
}

// String constants for bigquery destination config
const (
	GCPProjectID   = "project"
	GCPCredentials = "credentials"
	GCPLocation    = "location"
)

const (
	provider       = warehouseutils.BQ
	tableNameLimit = 127
)

// maps datatype stored in rudder to datatype in bigquery
var dataTypesMap = map[string]bigquery.FieldType{
	"boolean":  bigquery.BooleanFieldType,
	"int":      bigquery.IntegerFieldType,
	"float":    bigquery.FloatFieldType,
	"string":   bigquery.StringFieldType,
	"datetime": bigquery.TimestampFieldType,
}

// maps datatype in bigquery to datatype stored in rudder
var dataTypesMapToRudder = map[bigquery.FieldType]string{
	"BOOLEAN":   "boolean",
	"BOOL":      "boolean",
	"INTEGER":   "int",
	"INT64":     "int",
	"NUMERIC":   "float",
	"FLOAT":     "float",
	"FLOAT64":   "float",
	"STRING":    "string",
	"BYTES":     "string",
	"DATE":      "datetime",
	"DATETIME":  "datetime",
	"TIME":      "datetime",
	"TIMESTAMP": "datetime",
}

var primaryKeyMap = map[string]string{
	"users":                                "id",
	"identifies":                           "id",
	warehouseutils.DiscardsTable:           "row_id, column_name, table_name",
	warehouseutils.IdentityMappingsTable:   "merge_property_type, merge_property_value",
	warehouseutils.IdentityMergeRulesTable: "merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value",
}

var partitionKeyMap = map[string]string{
	"users":                                "id",
	"identifies":                           "id",
	warehouseutils.DiscardsTable:           "row_id, column_name, table_name",
	warehouseutils.IdentityMappingsTable:   "merge_property_type, merge_property_value",
	warehouseutils.IdentityMergeRulesTable: "merge_property_1_type, merge_property_1_value, merge_property_2_type, merge_property_2_value",
}

func getTableSchema(columns map[string]string) []*bigquery.FieldSchema {
	var schema []*bigquery.FieldSchema
	for columnName, columnType := range columns {
		schema = append(schema, &bigquery.FieldSchema{Name: columnName, Type: dataTypesMap[columnType]})
	}
	return schema
}

func (bq *HandleT) DeleteTable(tableName string) (err error) {
	tableRef := bq.db.Dataset(bq.namespace).Table(tableName)
	err = tableRef.Delete(bq.backgroundContext)
	return
}

func (bq *HandleT) CreateTable(tableName string, columnMap map[string]string) (err error) {
	pkgLogger.Infof("BQ: Creating table: %s in bigquery dataset: %s in project: %s", tableName, bq.namespace, bq.projectID)
	sampleSchema := getTableSchema(columnMap)
	metaData := &bigquery.TableMetadata{
		Schema:           sampleSchema,
		TimePartitioning: &bigquery.TimePartitioning{},
	}
	tableRef := bq.db.Dataset(bq.namespace).Table(tableName)
	err = tableRef.Create(bq.backgroundContext, metaData)
	if !checkAndIgnoreAlreadyExistError(err) {
		return
	}

	if !dedupEnabled() {
		err = bq.createTableView(tableName, columnMap)
	}
	return
}

func (bq *HandleT) DropTable(tableName string) (err error) {
	err = bq.DeleteTable(tableName)
	if err != nil {
		return
	}
	if !dedupEnabled() {
		err = bq.DeleteTable(tableName + "_view")
	}
	return
}

func (bq *HandleT) createTableView(tableName string, columnMap map[string]string) (err error) {
	partitionKey := "id"
	if column, ok := partitionKeyMap[tableName]; ok {
		partitionKey = column
	}

	var viewOrderByStmt string
	if _, ok := columnMap["loaded_at"]; ok {
		viewOrderByStmt = " ORDER BY loaded_at DESC "
	}

	// assuming it has field named id upon which dedup is done in view
	viewQuery := `SELECT * EXCEPT (__row_number) FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY ` + partitionKey + viewOrderByStmt + `) AS __row_number FROM ` + "`" + bq.projectID + "." + bq.namespace + "." + tableName + "`" + ` WHERE _PARTITIONTIME BETWEEN TIMESTAMP_TRUNC(TIMESTAMP_MICROS(UNIX_MICROS(CURRENT_TIMESTAMP()) - 60 * 60 * 60 * 24 * 1000000), DAY, 'UTC')
					AND TIMESTAMP_TRUNC(CURRENT_TIMESTAMP(), DAY, 'UTC')
			)
		WHERE __row_number = 1`
	metaData := &bigquery.TableMetadata{
		ViewQuery: viewQuery,
	}
	tableRef := bq.db.Dataset(bq.namespace).Table(tableName + "_view")
	err = tableRef.Create(bq.backgroundContext, metaData)
	return
}

func (bq *HandleT) addColumn(tableName, columnName, columnType string) (err error) {
	pkgLogger.Infof("BQ: Adding columns in table %s in bigquery dataset: %s in project: %s", tableName, bq.namespace, bq.projectID)
	tableRef := bq.db.Dataset(bq.namespace).Table(tableName)
	meta, err := tableRef.Metadata(bq.backgroundContext)
	if err != nil {
		return err
	}
	newSchema := append(meta.Schema,
		&bigquery.FieldSchema{Name: columnName, Type: dataTypesMap[columnType]},
	)
	update := bigquery.TableMetadataToUpdate{
		Schema: newSchema,
	}
	_, err = tableRef.Update(bq.backgroundContext, update, meta.ETag)
	return
}

func (bq *HandleT) schemaExists(_, _ string) (exists bool, err error) {
	ds := bq.db.Dataset(bq.namespace)
	_, err = ds.Metadata(bq.backgroundContext)
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok && e.Code == 404 {
			pkgLogger.Debugf("BQ: Dataset %s not found", bq.namespace)
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (bq *HandleT) CreateSchema() (err error) {
	pkgLogger.Infof("BQ: Creating bigquery dataset: %s in project: %s", bq.namespace, bq.projectID)
	location := strings.TrimSpace(warehouseutils.GetConfigValue(GCPLocation, bq.warehouse))
	if location == "" {
		location = "US"
	}

	var schemaExists bool
	schemaExists, err = bq.schemaExists(bq.namespace, location)
	if err != nil {
		pkgLogger.Errorf("BQ: Error checking if schema: %s exists: %v", bq.namespace, err)
		return err
	}
	if schemaExists {
		pkgLogger.Infof("BQ: Skipping creating schema: %s since it already exists", bq.namespace)
		return
	}

	ds := bq.db.Dataset(bq.namespace)
	meta := &bigquery.DatasetMetadata{
		Location: location,
	}
	pkgLogger.Infof("BQ: Creating schema: %s ...", bq.namespace)
	err = ds.Create(bq.backgroundContext, meta)
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok && e.Code == 409 {
			pkgLogger.Infof("BQ: Create schema %s failed as schema already exists", bq.namespace)
			return nil
		}
	}
	return
}

func checkAndIgnoreAlreadyExistError(err error) bool {
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok {
			// 409 is returned when we try to create a table that already exists
			// 400 is returned for all kinds of invalid input - so we need to check the error message too
			if e.Code == 409 || (e.Code == 400 && strings.Contains(e.Message, "already exists in schema")) {
				pkgLogger.Debugf("BQ: Google API returned error with code: %v", e.Code)
				return true
			}
		}
		return false
	}
	return true
}

func (bq *HandleT) dropStagingTable(stagingTableName string) {
	pkgLogger.Infof("BQ: Deleting table: %s in bigquery dataset: %s in project: %s", stagingTableName, bq.namespace, bq.projectID)
	err := bq.DeleteTable(stagingTableName)
	if err != nil {
		pkgLogger.Errorf("BQ:  Error dropping staging table %s in bigquery dataset %s in project %s : %v", stagingTableName, bq.namespace, bq.projectID, err)
	}
}

func (bq *HandleT) DeleteBy(tableNames []string, params warehouseutils.DeleteByParams) error {
	pkgLogger.Infof("BQ: Cleaning up the followng tables in bigquery for BQ:%s : %v", tableNames)
	for _, tb := range tableNames {
		sqlStatement := fmt.Sprintf(`DELETE FROM "%[1]s"."%[2]s" WHERE
		context_sources_job_run_id <> @jobrunid AND
		context_sources_task_run_id <> @taskrunid AND
		context_source_id = @sourceid AND
		received_at < @starttime`,
			bq.namespace,
			tb,
		)

		pkgLogger.Infof("PG: Deleting rows in table in bigquery for BQ:%s", bq.warehouse.Destination.ID)
		pkgLogger.Debugf("PG: Executing the sql statement %v", sqlStatement)
		query := bq.db.Query(sqlStatement)
		query.Parameters = []bigquery.QueryParameter{
			{Name: "jobrunid", Value: params.JobRunId},
			{Name: "taskrunid", Value: params.TaskRunId},
			{Name: "sourceid", Value: params.SourceId},
			{Name: "starttime", Value: params.StartTime},
		}
		if enableDeleteByJobs {
			job, err := query.Run(bq.backgroundContext)
			if err != nil {
				pkgLogger.Errorf("BQ: Error initiating load job: %v\n", err)
				return err
			}
			status, err := job.Wait(bq.backgroundContext)
			if err != nil {
				pkgLogger.Errorf("BQ: Error running job: %v\n", err)
				return err
			}
			if status.Err() != nil {
				return status.Err()
			}
		}

	}
	return nil
}

func partitionedTable(tableName, partitionDate string) string {
	return fmt.Sprintf(`%s$%v`, tableName, strings.ReplaceAll(partitionDate, "-", ""))
}

func (bq *HandleT) loadTable(tableName string, _, getLoadFileLocFromTableUploads, skipTempTableDelete bool) (stagingLoadTable StagingLoadTableT, err error) {
	pkgLogger.Infof("BQ: Starting load for table:%s\n", tableName)
	var loadFiles []warehouseutils.LoadFileT
	if getLoadFileLocFromTableUploads {
		loadFile, err := bq.uploader.GetSingleLoadFile(tableName)
		if err != nil {
			return stagingLoadTable, err
		}
		loadFiles = append(loadFiles, loadFile)
	} else {
		loadFiles = bq.uploader.GetLoadFilesMetadata(warehouseutils.GetLoadFilesOptionsT{Table: tableName})
	}
	gcsLocations := warehouseutils.GetGCSLocations(loadFiles, warehouseutils.GCSLocationOptionsT{})
	pkgLogger.Infof("BQ: Loading data into table: %s in bigquery dataset: %s in project: %s from %v", tableName, bq.namespace, bq.projectID, loadFiles)
	gcsRef := bigquery.NewGCSReference(gcsLocations...)
	gcsRef.SourceFormat = bigquery.JSON
	gcsRef.MaxBadRecords = 0
	gcsRef.IgnoreUnknownValues = false

	loadTableByAppend := func() (err error) {
		stagingLoadTable.partitionDate = time.Now().Format("2006-01-02")
		outputTable := tableName
		// Tables created by Rudderstack are ingestion-time partitioned table with pseudocolumn named _PARTITIONTIME. BigQuery automatically assigns rows to partitions based
		// on the time when BigQuery ingests the data. To support custom field partitions, omitting loading into partitioned table like tableName$20191221
		// TODO: Support custom field partition on users & identifies tables
		if !customPartitionsEnabled {
			outputTable = partitionedTable(tableName, stagingLoadTable.partitionDate)
		}

		loader := bq.db.Dataset(bq.namespace).Table(outputTable).LoaderFrom(gcsRef)

		job, err := loader.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating append load job: %v\n", err)
			return
		}
		status, err := job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error running append load job: %v\n", err)
			return
		}

		if status.Err() != nil {
			return status.Err()
		}
		return
	}

	loadTableByMerge := func() (err error) {
		stagingTableName := warehouseutils.StagingTableName(provider, tableName, tableNameLimit)
		stagingLoadTable.stagingTableName = stagingTableName
		pkgLogger.Infof("BQ: Loading data into temporary table: %s in bigquery dataset: %s in project: %s", stagingTableName, bq.namespace, bq.projectID)
		stagingTableColMap := bq.uploader.GetTableSchemaInWarehouse(tableName)
		sampleSchema := getTableSchema(stagingTableColMap)
		metaData := &bigquery.TableMetadata{
			Schema:           sampleSchema,
			TimePartitioning: &bigquery.TimePartitioning{},
		}
		tableRef := bq.db.Dataset(bq.namespace).Table(stagingTableName)
		err = tableRef.Create(bq.backgroundContext, metaData)
		if err != nil {
			pkgLogger.Infof("BQ: Error creating temporary staging table %s", stagingTableName)
			return
		}

		loader := bq.db.Dataset(bq.namespace).Table(stagingTableName).LoaderFrom(gcsRef)
		job, err := loader.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating staging table load job: %v\n", err)
			return
		}
		status, err := job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error running staging table load job: %v\n", err)
			return
		}

		if status.Err() != nil {
			return status.Err()
		}

		if !skipTempTableDelete {
			defer bq.dropStagingTable(stagingTableName)
		}

		primaryKey := "id"
		if column, ok := primaryKeyMap[tableName]; ok {
			primaryKey = column
		}

		partitionKey := "id"
		if column, ok := partitionKeyMap[tableName]; ok {
			partitionKey = column
		}

		tableColMap := bq.uploader.GetTableSchemaInWarehouse(tableName)
		var tableColNames []string
		for colName := range tableColMap {
			tableColNames = append(tableColNames, fmt.Sprintf("`%s`", colName))
		}

		var stagingColumnNamesList, columnsWithValuesList []string
		for _, str := range tableColNames {
			stagingColumnNamesList = append(stagingColumnNamesList, fmt.Sprintf(`staging.%s`, str))
			columnsWithValuesList = append(columnsWithValuesList, fmt.Sprintf(`original.%[1]s = staging.%[1]s`, str))
		}
		columnNames := strings.Join(tableColNames, ",")
		stagingColumnNames := strings.Join(stagingColumnNamesList, ",")
		columnsWithValues := strings.Join(columnsWithValuesList, ",")

		var primaryKeyList []string
		for _, str := range strings.Split(primaryKey, ",") {
			primaryKeyList = append(primaryKeyList, fmt.Sprintf(`original.%[1]s = staging.%[1]s`, strings.Trim(str, " ")))
		}
		primaryJoinClause := strings.Join(primaryKeyList, " AND ")
		bqTable := func(name string) string { return fmt.Sprintf("`%s`.`%s`", bq.namespace, name) }

		sqlStatement := fmt.Sprintf(`MERGE INTO %[1]s AS original
										USING (
											SELECT * FROM (
												SELECT *, row_number() OVER (PARTITION BY %[7]s ORDER BY RECEIVED_AT DESC) AS _rudder_staging_row_number FROM %[2]s
											) AS q WHERE _rudder_staging_row_number = 1
										) AS staging
										ON (%[3]s)
										WHEN MATCHED THEN
										UPDATE SET %[6]s
										WHEN NOT MATCHED THEN
										INSERT (%[4]s) VALUES (%[5]s)`, bqTable(tableName), bqTable(stagingTableName), primaryJoinClause, columnNames, stagingColumnNames, columnsWithValues, partitionKey)
		pkgLogger.Infof("BQ: Dedup records for table:%s using staging table: %s\n", tableName, sqlStatement)

		q := bq.db.Query(sqlStatement)
		job, err = q.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating merge load job: %v\n", err)
			return
		}
		status, err = job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error running merge load job: %v\n", err)
			return
		}

		if status.Err() != nil {
			return status.Err()
		}
		return
	}

	if !dedupEnabled() {
		err = loadTableByAppend()
		return
	}

	err = loadTableByMerge()
	return
}

func (bq *HandleT) LoadUserTables() (errorMap map[string]error) {
	errorMap = map[string]error{warehouseutils.IdentifiesTable: nil}
	pkgLogger.Infof("BQ: Starting load for identifies and users tables\n")
	identifyLoadTable, err := bq.loadTable(warehouseutils.IdentifiesTable, true, false, true)
	if err != nil {
		errorMap[warehouseutils.IdentifiesTable] = err
		return
	}

	if len(bq.uploader.GetTableSchemaInUpload(warehouseutils.UsersTable)) == 0 {
		return
	}
	errorMap[warehouseutils.UsersTable] = nil

	pkgLogger.Infof("BQ: Starting load for %s table", warehouseutils.UsersTable)

	firstValueSQL := func(column string) string {
		return fmt.Sprintf("FIRST_VALUE(`%[1]s` IGNORE NULLS) OVER (PARTITION BY id ORDER BY received_at DESC ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS `%[1]s`", column)
	}

	loadedAtFilter := func() string {
		// get first event received_at time in this upload for identifies table
		firstEventAt := func() time.Time {
			return bq.uploader.GetLoadFileGenStartTIme()
		}

		firstEventTime := firstEventAt()
		if !setUsersLoadPartitionFirstEventFilter || firstEventTime.IsZero() {
			return ""
		}

		// TODO: Add this filter to optimize reading from identifies table since first event in upload
		// rather than entire day's records
		// commented it since firstEventAt is not stored in UTC format in earlier versions
		firstEventAtFormatted := firstEventTime.Format(misc.RFC3339Milli)
		return fmt.Sprintf(`AND loaded_at >= TIMESTAMP('%v')`, firstEventAtFormatted)
	}

	userColMap := bq.uploader.GetTableSchemaInWarehouse("users")
	var userColNames, firstValProps []string
	for colName := range userColMap {
		if colName == "id" {
			continue
		}
		userColNames = append(userColNames, fmt.Sprintf("`%s`", colName))
		firstValProps = append(firstValProps, firstValueSQL(colName))
	}

	bqTable := func(name string) string { return fmt.Sprintf("`%s`.`%s`", bq.namespace, name) }

	bqUsersView := bqTable(warehouseutils.UsersView)
	viewExists, _ := bq.tableExists(warehouseutils.UsersView)
	if !viewExists {
		pkgLogger.Infof("BQ: Creating view: %s in bigquery dataset: %s in project: %s", warehouseutils.UsersView, bq.namespace, bq.projectID)
		bq.createTableView(warehouseutils.UsersTable, userColMap)
	}

	bqIdentifiesTable := bqTable(warehouseutils.IdentifiesTable)
	partition := fmt.Sprintf("TIMESTAMP('%s')", identifyLoadTable.partitionDate)
	var identifiesFrom string
	if dedupEnabled() {
		identifiesFrom = fmt.Sprintf(`%s WHERE user_id IS NOT NULL %s`, bqTable(identifyLoadTable.stagingTableName), loadedAtFilter())
	} else {
		identifiesFrom = fmt.Sprintf(`%s WHERE _PARTITIONTIME = %s AND user_id IS NOT NULL %s`, bqIdentifiesTable, partition, loadedAtFilter())
	}
	sqlStatement := fmt.Sprintf(`SELECT DISTINCT * FROM (
			SELECT id, %[1]s FROM (
				(
					SELECT id, %[2]s FROM %[3]s WHERE (
						id in (SELECT user_id FROM %[4]s)
					)
				) UNION ALL (
					SELECT user_id, %[2]s FROM %[4]s
				)
			)
		)`,
		strings.Join(firstValProps, ","), // 1
		strings.Join(userColNames, ","),  // 2
		bqUsersView,                      // 3
		identifiesFrom,                   // 4
	)
	loadUserTableByAppend := func() {
		pkgLogger.Infof(`BQ: Loading data into users table: %v`, sqlStatement)
		partitionedUsersTable := partitionedTable(warehouseutils.UsersTable, identifyLoadTable.partitionDate)
		query := bq.db.Query(sqlStatement)
		query.QueryConfig.Dst = bq.db.Dataset(bq.namespace).Table(partitionedUsersTable)
		query.WriteDisposition = bigquery.WriteAppend

		job, err := query.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating load job: %v\n", err)
			errorMap[warehouseutils.UsersTable] = err
			return
		}
		status, err := job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error running load job: %v\n", err)
			errorMap[warehouseutils.UsersTable] = fmt.Errorf(`append: %v`, err.Error())
			return
		}

		if status.Err() != nil {
			errorMap[warehouseutils.UsersTable] = status.Err()
			return
		}
	}

	loadUserTableByMerge := func() {
		stagingTableName := warehouseutils.StagingTableName(provider, warehouseutils.UsersTable, tableNameLimit)
		pkgLogger.Infof(`BQ: Creating staging table for users: %v`, sqlStatement)
		query := bq.db.Query(sqlStatement)
		query.QueryConfig.Dst = bq.db.Dataset(bq.namespace).Table(stagingTableName)
		query.WriteDisposition = bigquery.WriteAppend
		job, err := query.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating staging table for users : %v\n", err)
			errorMap[warehouseutils.UsersTable] = err
			return
		}

		status, err := job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating staging table for users %v\n", err)
			errorMap[warehouseutils.UsersTable] = fmt.Errorf(`merge: %v`, err.Error())
			return
		}

		if status.Err() != nil {
			errorMap[warehouseutils.UsersTable] = status.Err()
			return
		}
		defer bq.dropStagingTable(identifyLoadTable.stagingTableName)
		defer bq.dropStagingTable(stagingTableName)

		primaryKey := "ID"
		columnNames := append([]string{"ID"}, userColNames...)
		columnNamesStr := strings.Join(columnNames, ",")
		var columnsWithValues, stagingColumnValues string
		for idx, colName := range columnNames {
			columnsWithValues += fmt.Sprintf(`original.%[1]s = staging.%[1]s`, colName)
			stagingColumnValues += fmt.Sprintf(`staging.%s`, colName)
			if idx != len(columnNames)-1 {
				columnsWithValues += `,`
				stagingColumnValues += `,`
			}
		}

		sqlStatement = fmt.Sprintf(`MERGE INTO %[1]s AS original
										USING (
											SELECT %[3]s FROM %[2]s
										) AS staging
										ON (original.%[4]s = staging.%[4]s)
										WHEN MATCHED THEN
										UPDATE SET %[5]s
										WHEN NOT MATCHED THEN
										INSERT (%[3]s) VALUES (%[6]s)`, bqTable(warehouseutils.UsersTable), bqTable(stagingTableName), columnNamesStr, primaryKey, columnsWithValues, stagingColumnValues)
		pkgLogger.Infof("BQ: Dedup records for table:%s using staging table: %s\n", warehouseutils.UsersTable, sqlStatement)

		pkgLogger.Infof(`BQ: Loading data into users table: %v`, sqlStatement)
		// partitionedUsersTable := partitionedTable(warehouseutils.UsersTable, partitionDate)
		q := bq.db.Query(sqlStatement)
		job, err = q.Run(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error initiating merge load job: %v\n", err)
			errorMap[warehouseutils.UsersTable] = err
			return
		}
		status, err = job.Wait(bq.backgroundContext)
		if err != nil {
			pkgLogger.Errorf("BQ: Error running merge load job: %v\n", err)
			errorMap[warehouseutils.UsersTable] = fmt.Errorf(`merge: %v`, err.Error())
			return
		}

		if status.Err() != nil {
			errorMap[warehouseutils.UsersTable] = status.Err()
			return
		}
	}

	if !dedupEnabled() {
		loadUserTableByAppend()
		return
	}

	loadUserTableByMerge()
	return errorMap
}

type BQCredentialsT struct {
	ProjectID   string
	Credentials string
}

func Connect(context context.Context, cred *BQCredentialsT) (*bigquery.Client, error) {
	opts := []option.ClientOption{}
	if !googleutils.ShouldSkipCredentialsInit(cred.Credentials) {
		credBytes := []byte(cred.Credentials)
		if err := googleutils.CompatibleGoogleCredentialsJSON(credBytes); err != nil {
			return nil, err
		}
		opts = append(opts, option.WithCredentialsJSON(credBytes))
	}
	client, err := bigquery.NewClient(context, cred.ProjectID, opts...)
	return client, err
}

func (bq *HandleT) connect(cred BQCredentialsT) (*bigquery.Client, error) {
	pkgLogger.Infof("BQ: Connecting to BigQuery in project: %s", cred.ProjectID)
	bq.backgroundContext = context.Background()
	client, err := Connect(bq.backgroundContext, &cred)
	return client, err
}

func loadConfig() {
	config.RegisterBoolConfigVariable(true, &setUsersLoadPartitionFirstEventFilter, true, "Warehouse.bigquery.setUsersLoadPartitionFirstEventFilter")
	config.RegisterBoolConfigVariable(false, &customPartitionsEnabled, true, "Warehouse.bigquery.customPartitionsEnabled")
	config.RegisterBoolConfigVariable(false, &isUsersTableDedupEnabled, true, "Warehouse.bigquery.isUsersTableDedupEnabled") // TODO: Depricate with respect to isDedupEnabled
	config.RegisterBoolConfigVariable(false, &isDedupEnabled, true, "Warehouse.bigquery.isDedupEnabled")
	config.RegisterBoolConfigVariable(false, &enableDeleteByJobs, true, "Warehouse.bigquery.enableDeleteByJobs")
}

func Init() {
	loadConfig()
	pkgLogger = logger.NewLogger().Child("warehouse").Child("bigquery")
}

func dedupEnabled() bool {
	return isDedupEnabled || isUsersTableDedupEnabled
}

func (bq *HandleT) CrashRecover(warehouse warehouseutils.Warehouse) (err error) {
	if !dedupEnabled() {
		return
	}
	bq.warehouse = warehouse
	bq.namespace = warehouse.Namespace
	bq.projectID = strings.TrimSpace(warehouseutils.GetConfigValue(GCPProjectID, bq.warehouse))
	bq.db, err = bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
	})
	if err != nil {
		return
	}
	defer bq.db.Close()
	bq.dropDanglingStagingTables()
	return
}

func (bq *HandleT) dropDanglingStagingTables() bool {
	sqlStatement := fmt.Sprintf(`
		SELECT
		  table_name
		FROM
		  %[1]s.INFORMATION_SCHEMA.TABLES
		WHERE
		  table_schema = '%[1]s'
		  AND table_name LIKE '%[2]s';
	`,
		bq.namespace,
		fmt.Sprintf(`%s%%`, warehouseutils.StagingTablePrefix(provider)),
	)
	query := bq.db.Query(sqlStatement)
	it, err := query.Read(bq.backgroundContext)
	if err != nil {
		pkgLogger.Errorf("WH: BQ: Error dropping dangling staging tables in BQ: %v\nQuery: %s\n", err, sqlStatement)
		return false
	}

	var stagingTableNames []string
	for {
		var values []bigquery.Value
		err := it.Next(&values)
		if err == iterator.Done {
			break
		}
		if err != nil {
			pkgLogger.Errorf("BQ: Error in processing fetched staging tables from information schema in dataset %v : %v", bq.namespace, err)
			return false
		}
		if _, ok := values[0].(string); ok {
			stagingTableNames = append(stagingTableNames, values[0].(string))
		}
	}
	pkgLogger.Infof("WH: PG: Dropping dangling staging tables: %+v  %+v\n", len(stagingTableNames), stagingTableNames)
	delSuccess := true
	for _, stagingTableName := range stagingTableNames {
		err := bq.DeleteTable(stagingTableName)
		if err != nil {
			pkgLogger.Errorf("WH: BQ:  Error dropping dangling staging table: %s in BQ: %v", stagingTableName, err)
			delSuccess = false
		}
	}
	return delSuccess
}

func (bq *HandleT) IsEmpty(warehouse warehouseutils.Warehouse) (empty bool, err error) {
	empty = true
	bq.warehouse = warehouse
	bq.namespace = warehouse.Namespace
	bq.projectID = strings.TrimSpace(warehouseutils.GetConfigValue(GCPProjectID, bq.warehouse))
	pkgLogger.Infof("BQ: Connecting to BigQuery in project: %s", bq.projectID)
	bq.db, err = bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
	})
	if err != nil {
		return
	}
	defer bq.db.Close()

	tables := []string{"tracks", "pages", "screens", "identifies", "aliases"}
	for _, tableName := range tables {
		var exists bool
		exists, err = bq.tableExists(tableName)
		if err != nil {
			return
		}
		if !exists {
			continue
		}
		count, err := bq.GetTotalCountInTable(bq.backgroundContext, tableName)
		if err != nil {
			return empty, err
		}
		if count > 0 {
			empty = false
			return empty, nil
		}
	}
	return
}

func (bq *HandleT) Setup(warehouse warehouseutils.Warehouse, uploader warehouseutils.UploaderI) (err error) {
	bq.warehouse = warehouse
	bq.namespace = warehouse.Namespace
	bq.uploader = uploader
	bq.projectID = strings.TrimSpace(warehouseutils.GetConfigValue(GCPProjectID, bq.warehouse))

	bq.backgroundContext = context.Background()
	bq.db, err = bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
	})
	return err
}

func (bq *HandleT) TestConnection(warehouse warehouseutils.Warehouse) (err error) {
	bq.warehouse = warehouse
	bq.db, err = bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
	})
	if err != nil {
		return
	}
	defer bq.db.Close()
	return
}

func (bq *HandleT) LoadTable(tableName string) error {
	var getLoadFileLocFromTableUploads bool
	switch tableName {
	case warehouseutils.IdentityMappingsTable, warehouseutils.IdentityMergeRulesTable:
		getLoadFileLocFromTableUploads = true
	default:
		getLoadFileLocFromTableUploads = false
	}
	_, err := bq.loadTable(tableName, false, getLoadFileLocFromTableUploads, false)
	return err
}

func (bq *HandleT) AddColumn(tableName, columnName, columnType string) (err error) {
	err = bq.addColumn(tableName, columnName, columnType)
	if err != nil {
		if checkAndIgnoreAlreadyExistError(err) {
			pkgLogger.Infof("BQ: Column %s already exists on %s.%s \nResponse: %v", columnName, bq.namespace, tableName, err)
			err = nil
		}
	}
	return err
}

func (*HandleT) AlterColumn(_, _, _ string) (err error) {
	return
}

// FetchSchema queries bigquery and returns the schema assoiciated with provided namespace
func (bq *HandleT) FetchSchema(warehouse warehouseutils.Warehouse) (schema warehouseutils.SchemaT, err error) {
	bq.warehouse = warehouse
	bq.namespace = warehouse.Namespace
	bq.projectID = strings.TrimSpace(warehouseutils.GetConfigValue(GCPProjectID, bq.warehouse))
	dbClient, err := bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
		// location:    warehouseutils.GetConfigValue(GCPLocation, bq.warehouse),
	})
	if err != nil {
		return
	}
	defer dbClient.Close()

	schema = make(warehouseutils.SchemaT)
	sqlStatement := fmt.Sprintf(`
		SELECT
		  t.table_name,
		  c.column_name,
		  c.data_type
		FROM
		  %[1]s.INFORMATION_SCHEMA.TABLES as t
		  LEFT JOIN %[1]s.INFORMATION_SCHEMA.COLUMNS as c ON (t.table_name = c.table_name)
		WHERE
		  (t.table_type != 'VIEW')
		  and (
			c.column_name != '_PARTITIONTIME'
			OR c.column_name IS NULL
		  )
	`,
		bq.namespace,
	)
	query := dbClient.Query(sqlStatement)

	it, err := query.Read(bq.backgroundContext)
	if err != nil {
		if e, ok := err.(*googleapi.Error); ok {
			// if dataset resource is not found, return empty schema
			if e.Code == 404 {
				pkgLogger.Infof("BQ: No rows, while fetching schema from  destination:%v, query: %v", bq.warehouse.Identifier, query)
				return schema, nil
			}
			pkgLogger.Errorf("BQ: Error in fetching schema from bigquery destination:%v, query: %v", bq.warehouse.Destination.ID, query)
			return schema, e
		}
		pkgLogger.Errorf("BQ: Error in fetching schema from bigquery destination:%v, query: %v", bq.warehouse.Destination.ID, query)
		return
	}

	for {
		var values []bigquery.Value
		err := it.Next(&values)
		if err == iterator.Done {
			break
		}
		if err != nil {
			pkgLogger.Errorf("BQ: Error in processing fetched schema from redshift destination:%v, error: %v", bq.warehouse.Destination.ID, err)
			return nil, err
		}
		var tName, cName, cType string
		tName, _ = values[0].(string)
		if _, ok := schema[tName]; !ok {
			schema[tName] = make(map[string]string)
		}
		cName, _ = values[1].(string)
		cType, _ = values[2].(string)
		if datatype, ok := dataTypesMapToRudder[bigquery.FieldType(cType)]; ok {
			// lower case all column names from bigquery
			schema[tName][strings.ToLower(cName)] = datatype
		} else {
			warehouseutils.WHCounterStat(warehouseutils.RUDDER_MISSING_DATATYPE, &bq.warehouse, warehouseutils.Tag{Name: "datatype", Value: cType}).Count(1)
		}
	}

	return
}

func (bq *HandleT) Cleanup() {
	if bq.db != nil {
		bq.db.Close()
	}
}

func (bq *HandleT) LoadIdentityMergeRulesTable() (err error) {
	identityMergeRulesTable := warehouseutils.IdentityMergeRulesWarehouseTableName(warehouseutils.BQ)
	return bq.LoadTable(identityMergeRulesTable)
}

func (bq *HandleT) LoadIdentityMappingsTable() (err error) {
	identityMappingsTable := warehouseutils.IdentityMappingsWarehouseTableName(warehouseutils.BQ)
	return bq.LoadTable(identityMappingsTable)
}

func (bq *HandleT) tableExists(tableName string) (exists bool, err error) {
	_, err = bq.db.Dataset(bq.namespace).Table(tableName).Metadata(context.Background())
	if err == nil {
		return true, nil
	}
	if e, ok := err.(*googleapi.Error); ok {
		if e.Code == 404 {
			return false, nil
		}
	}
	return false, err
}

func (bq *HandleT) columnExists(columnName, tableName string) (exists bool, err error) {
	tableMetadata, err := bq.db.Dataset(bq.namespace).Table(tableName).Metadata(context.Background())
	if err != nil {
		return false, err
	}

	schema := tableMetadata.Schema
	for _, column := range schema {
		if column.Name == columnName {
			return true, nil
		}
	}

	return false, nil
}

type identityRulesT struct {
	MergeProperty1Type  string `json:"merge_property_1_type"`
	MergeProperty1Value string `json:"merge_property_1_value"`
	MergeProperty2Type  string `json:"merge_property_2_type"`
	MergeProperty2Value string `json:"merge_property_2_value"`
}

func (bq *HandleT) DownloadIdentityRules(gzWriter *misc.GZipWriter) (err error) {
	getFromTable := func(tableName string) (err error) {
		var exists bool
		exists, err = bq.tableExists(tableName)
		if err != nil || !exists {
			return
		}

		tableMetadata, err := bq.db.Dataset(bq.namespace).Table(tableName).Metadata(context.Background())
		if err != nil {
			return err
		}
		totalRows := int64(tableMetadata.NumRows)
		// check if table in warehouse has anonymous_id and user_id and construct accordingly
		hasAnonymousID, err := bq.columnExists("anonymous_id", tableName)
		if err != nil {
			return
		}
		hasUserID, err := bq.columnExists("user_id", tableName)
		if err != nil {
			return
		}

		var toSelectFields string
		if hasAnonymousID && hasUserID {
			toSelectFields = `anonymous_id, user_id`
		} else if hasAnonymousID {
			toSelectFields = `anonymous_id, null as user_id`
		} else if hasUserID {
			toSelectFields = `null as anonymous_id", user_id`
		} else {
			pkgLogger.Infof("BQ: anonymous_id, user_id columns not present in table: %s", tableName)
			return nil
		}

		batchSize := int64(10000)
		var offset int64
		for {
			sqlStatement := fmt.Sprintf(`SELECT DISTINCT %[1]s FROM %[2]s.%[3]s LIMIT %[4]d OFFSET %[5]d`, toSelectFields, bq.namespace, tableName, batchSize, offset)
			pkgLogger.Infof("BQ: Downloading distinct combinations of anonymous_id, user_id: %s, totalRows: %d", sqlStatement, totalRows)
			ctx := context.Background()
			query := bq.db.Query(sqlStatement)
			job, err := query.Run(ctx)
			if err != nil {
				break
			}
			status, err := job.Wait(ctx)
			if err != nil {
				return err
			}
			if err := status.Err(); err != nil {
				return err
			}
			it, err := job.Read(ctx)
			if err != nil {
				return err
			}
			for {
				var values []bigquery.Value

				err := it.Next(&values)
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				var anonId, userId string
				if _, ok := values[0].(string); ok {
					anonId = values[0].(string)
				}
				if _, ok := values[1].(string); ok {
					userId = values[1].(string)
				}
				identityRule := identityRulesT{
					MergeProperty1Type:  "anonymous_id",
					MergeProperty1Value: anonId,
					MergeProperty2Type:  "user_id",
					MergeProperty2Value: userId,
				}
				if identityRule.MergeProperty1Value == "" && identityRule.MergeProperty2Value == "" {
					continue
				}
				bytes, err := json.Marshal(identityRule)
				if err != nil {
					break
				}
				gzWriter.WriteGZ(string(bytes) + "\n")
			}

			offset += batchSize
			if offset >= totalRows {
				break
			}
		}
		return
	}

	tables := []string{"tracks", "pages", "screens", "identifies", "aliases"}
	for _, table := range tables {
		err = getFromTable(table)
		if err != nil {
			return
		}
	}
	return
}

func (bq *HandleT) GetTotalCountInTable(ctx context.Context, tableName string) (total int64, err error) {
	sqlStatement := fmt.Sprintf(`SELECT count(*) FROM %[1]s.%[2]s`, bq.namespace, tableName)
	it, err := bq.db.Query(sqlStatement).Read(ctx)
	if err != nil {
		return 0, err
	}
	var values []bigquery.Value
	err = it.Next(&values)
	if err == iterator.Done {
		return 0, nil
	}
	if err != nil {
		pkgLogger.Errorf("BQ: Error in processing totalRowsCount: %v", err)
		return
	}
	total, _ = values[0].(int64)
	return
}

func (bq *HandleT) Connect(warehouse warehouseutils.Warehouse) (client.Client, error) {
	bq.warehouse = warehouse
	bq.namespace = warehouse.Namespace
	bq.projectID = strings.TrimSpace(warehouseutils.GetConfigValue(GCPProjectID, bq.warehouse))
	dbClient, err := bq.connect(BQCredentialsT{
		ProjectID:   bq.projectID,
		Credentials: warehouseutils.GetConfigValue(GCPCredentials, bq.warehouse),
	})
	if err != nil {
		return client.Client{}, err
	}

	return client.Client{Type: client.BQClient, BQ: dbClient}, err
}

func (bq *HandleT) LoadTestTable(location, tableName string, _ map[string]interface{}, _ string) (err error) {
	gcsLocations := warehouseutils.GetGCSLocation(location, warehouseutils.GCSLocationOptionsT{})
	gcsRef := bigquery.NewGCSReference([]string{gcsLocations}...)
	gcsRef.SourceFormat = bigquery.JSON
	gcsRef.MaxBadRecords = 0
	gcsRef.IgnoreUnknownValues = false

	outputTable := partitionedTable(tableName, time.Now().Format("2006-01-02"))
	loader := bq.db.Dataset(bq.namespace).Table(outputTable).LoaderFrom(gcsRef)

	job, err := loader.Run(bq.backgroundContext)
	if err != nil {
		return
	}
	status, err := job.Wait(bq.backgroundContext)
	if err != nil {
		return
	}

	if status.Err() != nil {
		err = status.Err()
		return
	}
	return
}

func (*HandleT) SetConnectionTimeout(_ time.Duration) {
}
