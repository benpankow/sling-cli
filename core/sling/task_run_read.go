package sling

import (
	"bufio"
	"os"
	"strings"

	"github.com/flarco/g"
	"github.com/samber/lo"
	"github.com/slingdata-io/sling-cli/core/dbio"
	"github.com/slingdata-io/sling-cli/core/dbio/database"
	"github.com/slingdata-io/sling-cli/core/dbio/filesys"
	"github.com/slingdata-io/sling-cli/core/dbio/iop"
	"github.com/spf13/cast"
)

// ReadFromDB reads from a source database
func (t *TaskExecution) ReadFromDB(cfg *Config, srcConn database.Connection) (df *iop.Dataflow, err error) {

	setStage("3 - prepare-dataflow")

	selectFieldsStr := "*"
	sTable, err := database.ParseTableName(cfg.Source.Stream, srcConn.GetType())
	if err != nil {
		err = g.Error(err, "Could not parse source stream text")
		return t.df, err
	} else if sTable.Schema == "" {
		sTable.Schema = cast.ToString(cfg.Source.Data["schema"])
	}

	// get source columns
	st := sTable
	st.SQL = g.R(st.SQL, "incremental_where_cond", "1=1") // so we get the columns, and not change the orig SQL
	st.SQL = g.R(st.SQL, "incremental_value", "null")     // so we get the columns, and not change the orig SQL
	sTable.Columns, err = srcConn.GetSQLColumns(st)
	if err != nil {
		err = g.Error(err, "Could not get source columns")
		return t.df, err
	}

	if len(cfg.Source.Select) > 0 {
		fields := lo.Map(cfg.Source.Select, func(f string, i int) string {
			return f
		})

		excluded := lo.Filter(cfg.Source.Select, func(f string, i int) bool {
			return strings.HasPrefix(f, "-")
		})

		if len(excluded) > 0 {
			if len(excluded) != len(cfg.Source.Select) {
				return t.df, g.Error("All specified select columns must be excluded with prefix '-'. Cannot do partial exclude.")
			}

			q := database.GetQualifierQuote(srcConn.GetType())
			includedCols := lo.Filter(sTable.Columns, func(c iop.Column, i int) bool {
				for _, exField := range excluded {
					exField = strings.ReplaceAll(strings.TrimPrefix(exField, "-"), q, "")
					if strings.EqualFold(c.Name, exField) {
						return false
					}
				}
				return true
			})

			if len(includedCols) == 0 {
				return t.df, g.Error("All available columns were excluded")
			}
			fields = iop.Columns(includedCols).Names()
		}

		selectFieldsStr = strings.Join(fields, ", ")
	}

	if t.usingCheckpoint() || t.Config.Mode == BackfillMode {
		// default true value
		incrementalWhereCond := "1=1"

		// get source columns to match update-key
		// in case column casing needs adjustment
		updateCol := sTable.Columns.GetColumn(cfg.Source.UpdateKey)
		if updateCol.Name != "" {
			cfg.Source.UpdateKey = updateCol.Name // overwrite with correct casing
		}

		// select only records that have been modified after last max value
		if cfg.IncrementalVal != nil {
			// if primary key is defined, use greater than or equal
			// in case that many timestamp values are the same and
			// IncrementalVal has been truncated in target database system
			greaterThan := lo.Ternary(t.Config.Source.HasPrimaryKey(), ">=", ">")

			incrementalWhereCond = g.R(
				srcConn.GetTemplateValue("core.incremental_where"),
				"update_key", srcConn.Quote(cfg.Source.UpdateKey, false),
				"value", cfg.IncrementalValStr,
				"gt", greaterThan,
			)
		} else {
			// allows the use of coalesce in custom SQL using {incremental_value}
			// this will be null when target table does not exists
			cfg.IncrementalValStr = "null"
		}

		if t.Config.Mode == BackfillMode {
			rangeArr := strings.Split(*cfg.Source.Options.Range, ",")
			startValue := rangeArr[0]
			endValue := rangeArr[1]

			// oracle's DATE type is mapped to datetime, but needs to use the TO_DATE function
			isOracleDate := updateCol.DbType == "DATE" && srcConn.GetType() == dbio.TypeDbOracle

			if updateCol.IsDate() || isOracleDate {
				timestampTemplate := srcConn.GetTemplateValue("variable.date_layout_str")
				startValue = g.R(timestampTemplate, "value", startValue)
				endValue = g.R(timestampTemplate, "value", endValue)
			} else if updateCol.IsDatetime() {
				timestampTemplate := srcConn.GetTemplateValue("variable.timestamp_layout_str")
				startValue = g.R(timestampTemplate, "value", startValue)
				endValue = g.R(timestampTemplate, "value", endValue)
			} else if updateCol.IsString() {
				startValue = `'` + startValue + `'`
				endValue = `'` + endValue + `'`
			}

			incrementalWhereCond = g.R(
				srcConn.GetTemplateValue("core.backfill_where"),
				"update_key", srcConn.Quote(cfg.Source.UpdateKey, false),
				"start_value", startValue,
				"end_value", endValue,
			)
		}

		if sTable.SQL == "" {
			sTable.SQL = g.R(
				srcConn.GetTemplateValue("core.incremental_select"),
				"fields", selectFieldsStr,
				"table", sTable.FDQN(),
				"incremental_where_cond", incrementalWhereCond,
				"update_key", srcConn.Quote(cfg.Source.UpdateKey, false),
			)
		} else {
			if !(strings.Contains(sTable.SQL, "{incremental_where_cond}") || strings.Contains(sTable.SQL, "{incremental_value}")) {
				err = g.Error("Since using incremental/backfill mode + custom SQL, with an `update_key`, the SQL text needs to contain a placeholder: {incremental_where_cond} or {incremental_value}. See https://docs.slingdata.io for help.")
				return t.df, err
			}

			sTable.SQL = g.R(
				sTable.SQL,
				"incremental_where_cond", incrementalWhereCond,
				"update_key", srcConn.Quote(cfg.Source.UpdateKey, false),
				"incremental_value", cfg.IncrementalValStr,
			)
		}
	}

	if srcConn.GetType() == dbio.TypeDbBigTable {
		srcConn.SetProp("start_time", t.Config.IncrementalValStr)
	}

	sTable.SQL = g.R(sTable.SQL, "incremental_where_cond", "1=1") // if running non-incremental mode
	sTable.SQL = g.R(sTable.SQL, "incremental_value", "null")     // if running non-incremental mode

	// construct select statement for selected fields
	if selectFieldsStr != "*" || cfg.Source.Limit() > 0 {
		sTable.SQL = sTable.Select(cfg.Source.Limit(), cfg.Source.Offset(), strings.Split(selectFieldsStr, ",")...)
	}

	df, err = srcConn.BulkExportFlow(sTable)
	if err != nil {
		err = g.Error(err, "Could not BulkExportFlow")
		return t.df, err
	}

	err = t.setColumnKeys(df)
	if err != nil {
		err = g.Error(err, "Could not set column keys")
		return t.df, err
	}

	g.Trace("%#v", df.Columns.Types())
	setStage("3 - dataflow-stream")

	return
}

// ReadFromFile reads from a source file
func (t *TaskExecution) ReadFromFile(cfg *Config) (df *iop.Dataflow, err error) {

	setStage("3 - prepare-dataflow")

	// sets metadata
	metadata := t.setGetMetadata()

	var stream *iop.Datastream
	options := t.sourceOptionsMap()
	options["METADATA"] = g.Marshal(metadata)

	if t.Config.IncrementalVal != nil {
		// file stream incremental mode
		if t.Config.Source.UpdateKey == slingLoadedAtColumn {
			options["SLING_FS_TIMESTAMP"] = t.Config.IncrementalVal
			g.Debug(`file stream using file_sys_timestamp=%#v and update_key=%s`, t.Config.IncrementalVal, t.Config.Source.UpdateKey)
		} else {
			options["SLING_INCREMENTAL_COL"] = t.Config.Source.UpdateKey
			options["SLING_INCREMENTAL_VAL"] = t.Config.IncrementalVal
			g.Debug(`file stream using incremental_val=%#v and update_key=%s`, t.Config.IncrementalVal, t.Config.Source.UpdateKey)
		}
	}

	if uri := cfg.SrcConn.URL(); uri != "" {
		// construct props by merging with options
		props := append(
			g.MapToKVArr(cfg.SrcConn.DataS()),
			g.MapToKVArr(g.ToMapString(options))...,
		)

		fs, err := filesys.NewFileSysClientFromURLContext(t.Context.Ctx, uri, props...)
		if err != nil {
			err = g.Error(err, "Could not obtain client for %s ", cfg.SrcConn.Type)
			return t.df, err
		}

		fsCfg := filesys.FileStreamConfig{Select: cfg.Source.Select, Limit: cfg.Source.Limit()}
		df, err = fs.ReadDataflow(uri, fsCfg)
		if err != nil {
			err = g.Error(err, "Could not FileSysReadDataflow for %s", cfg.SrcConn.Type)
			return t.df, err
		}
	} else {
		stream, err = filesys.MakeDatastream(bufio.NewReader(os.Stdin), g.ToMapString(options))
		if err != nil {
			err = g.Error(err, "Could not MakeDatastream")
			return t.df, err
		}
		df, err = iop.MakeDataFlow(stream.Split()...)
		if err != nil {
			err = g.Error(err, "Could not MakeDataFlow for Stdin")
			return t.df, err
		}
	}

	if len(df.Streams) == 0 {
		streamName := lo.Ternary(cfg.SrcConn.URL() == "", "stdin", cfg.SrcConn.URL())
		return df, g.Error("Could not read stream (%s)", streamName)
	} else if len(df.Columns) == 0 && !df.Streams[0].IsClosed() {
		return df, g.Error("Could not read columns")
	}

	err = t.setColumnKeys(df)
	if err != nil {
		err = g.Error(err, "Could not set column keys")
		return t.df, err
	}

	g.Trace("%#v", df.Columns.Types())
	setStage("3 - dataflow-stream")

	return
}

// setColumnKeys sets the column keys
func (t *TaskExecution) setColumnKeys(df *iop.Dataflow) (err error) {
	eG := g.ErrorGroup{}

	if t.Config.Source.HasPrimaryKey() {
		// set true PK only when StarRocks, we don't want to create PKs on target table implicitly
		if t.Config.Source.Type == dbio.TypeDbStarRocks {
			eG.Capture(df.Columns.SetKeys(iop.PrimaryKey, t.Config.Source.PrimaryKey()...))
		}
		eG.Capture(df.Columns.SetMetadata(iop.PrimaryKey.MetadataKey(), "source", t.Config.Source.PrimaryKey()...))
	}

	if t.Config.Source.HasUpdateKey() {
		eG.Capture(df.Columns.SetMetadata(iop.UpdateKey.MetadataKey(), "source", t.Config.Source.UpdateKey))
	}

	if tkMap := t.Config.Target.Options.TableKeys; tkMap != nil {
		for tableKey, keys := range tkMap {
			eG.Capture(df.Columns.SetKeys(tableKey, keys...))
		}
	}

	return eG.Err()
}
