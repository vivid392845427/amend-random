package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-units"
	_ "github.com/go-sql-driver/mysql"
	"github.com/juju/errors"
	"github.com/you06/amend-random/check"
	"github.com/you06/go-mikadzuki/kv"
	"github.com/you06/go-mikadzuki/util"
	"github.com/zyguan/tidb-test-util/pkg/result"
)

var (
	columnLeast     = 2
	columnMost      = 200
	colCnt          = 0
	ddlCnt          = 10
	dmlCnt          = 10
	dmlThread       = 20
	batchSize       = 10
	totalRound      = 0
	txnSizeStr      = ""
	txnSize         int64
	dsn1            = ""
	dsn2            = ""
	mode            = ""
	dmlExecutorName = ""
	dmlExecutor     DMLExecutor
	selectedModes   []string
	modeMap         = map[string]ddlRandom{
		"create-index":        CreateIndex,
		"drop-index":          DropIndex,
		"create-unique-index": CreateUniqueIndex,
		"drop-unique-index":   DropUniqueIndex,
		"add-column":          AddColumn,
		"drop-column":         DropColumn,
		"change-column-size":  ChangeColumnSize,
		"change-column-type":  ChangeColumnType,
	}
	dmlExecutors = map[string]DMLExecutor{
		"update-conflict": UpdateConflictExecutor,
		"insert-update":   InsertUpdateExecutor,
	}
	modeFns        []ddlRandom
	tableName      = "t"
	checkTableName = ""
	checkOnly      bool
	mbSize         = 1048576
	timeout        time.Duration
	dbnamePattern  = regexp.MustCompile(`^(.*\/)([0-9a-zA-Z_]+)\??.*$`)
	dsn1NoDB       string
	dsn2NoDB       string
	dbname         string
	failfast       bool
	clusteredIndex bool
)

func init() {
	var (
		supportedMode     []string
		supportedExecutor []string
	)
	for k := range modeMap {
		supportedMode = append(supportedMode, k)
	}
	for k := range dmlExecutors {
		supportedExecutor = append(supportedExecutor, k)
	}
	flag.IntVar(&ddlCnt, "ddl-count", 10, "ddl count, one ddl execution increase at least one schema diff")
	flag.IntVar(&dmlCnt, "dml-count", 10, "dml count")
	flag.IntVar(&dmlThread, "dml-thread", 20, "dml thread")
	flag.StringVar(&dsn1, "dsn1", "root:@tcp(127.0.0.1:4000)/test?tidb_enable_amend_pessimistic_txn=1", "upstream dsn")
	flag.StringVar(&dsn2, "dsn2", "", "downstream dsn")
	flag.StringVar(&mode, "mode", "", fmt.Sprintf("ddl modes, split with \",\", supportted modes: %s", strings.Join(supportedMode, ",")))
	flag.StringVar(&dmlExecutorName, "executor", supportedExecutor[0], fmt.Sprintf("dml executor, supportted executors: %s", strings.Join(supportedExecutor, ",")))
	flag.StringVar(&tableName, "tablename", "t", "tablename")
	flag.BoolVar(&checkOnly, "checkonly", false, "only check diff")
	flag.StringVar(&txnSizeStr, "txn-size", "", "the estimated txn's size, will overwrite dml-count, eg. 100M, 1G")
	flag.IntVar(&totalRound, "round", 0, "exec round, 0 means infinite execution")
	flag.IntVar(&batchSize, "batch", 10, "batch size of insert, 0 for auto")
	flag.DurationVar(&timeout, "timeout", 10*time.Minute, "execution phase timeout for each round")
	flag.BoolVar(&failfast, "failfast", true, "exit immediately on the first failure")
	flag.BoolVar(&clusteredIndex, "clusteredIndex", false, "if the clustered index is used for the test table")

	rand.Seed(time.Now().UnixNano())
	flag.Parse()

	checkTableName = fmt.Sprintf("check_point_%s", tableName)
}

func initMode() error {
	selectedModes = strings.Split(mode, ",")
	set := make(map[string]struct{})
	for i, m := range selectedModes {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		fn, ok := modeMap[m]
		if !ok {
			return errors.Errorf("mode %s not supportted", m)
		}

		selectedModes[i] = m
		if _, ok := set[m]; !ok {
			modeFns = append(modeFns, fn)
		}
		set[m] = struct{}{}
	}
	if len(modeFns) == 0 {
		fmt.Println("[WARN] no sql mode is selected, there will be DML only")
	}
	if txnSizeStr != "" {
		var err error
		txnSize, err = units.RAMInBytes(txnSizeStr)
		if err != nil {
			return err
		}
	}
	if e, ok := dmlExecutors[dmlExecutorName]; !ok {
		return errors.Errorf("invalid dml executor name `%s`", dmlExecutorName)
	} else {
		dmlExecutor = e
	}
	dsn1NoDB = dbnamePattern.FindStringSubmatch(dsn1)[1]
	dsn2NoDB = dbnamePattern.FindStringSubmatch(dsn2)[1]
	dbname = dbnamePattern.FindStringSubmatch(dsn1)[2]
	return nil
}

func main() {
	if checkOnly {
		db1, err := sql.Open("mysql", dsn1)
		if err != nil {
			panic(err)
		}
		db2, err := sql.Open("mysql", dsn2)
		if err != nil {
			panic(err)
		}
		if err := check.Check(db1, db2, tableName); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("check pass, data same.")
		}
		return
	}

	if err := initMode(); err != nil {
		panic(err)
	}

	db1, err := sql.Open("mysql", dsn1NoDB)
	if err != nil {
		panic(err)
	}
	var db2 *sql.DB
	if dsn2 != "" {
		db2, err = sql.Open("mysql", dsn2NoDB)
		if err != nil {
			panic(err)
		}
	}

	round, conclusion, output := 0, result.Success, ""
	result.InitDefault()
	defer func() {
		result.Report(conclusion, output)
		if conclusion != result.Success {
			os.Exit(1)
		}
	}()
	for {
		round++
		fmt.Println("round:", round)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		errCh := make(chan error, 1)
		log := newLog()

		go func() {
			MustExec(db1, dsn1NoDB, fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbname), true, "")
			MustExec(db1, dsn1NoDB, fmt.Sprintf("CREATE DATABASE %s", dbname), true, "database exists")
			db1, err = sql.Open("mysql", dsn1)
			if err != nil {
				panic(err)
			}
			MustExec(db1, dsn1, fmt.Sprintf("DROP TABLE IF EXISTS %s", checkTableName), true, "")
			MustExec(db1, dsn1, fmt.Sprintf("CREATE TABLE %s(id int)", checkTableName), true, "already exists")
			errCh <- once(db1, db2, &log)
		}()

		select {
		case <-ctx.Done():
			fmt.Printf("batch timeout after %s, dumping log...\n", timeout)
			fmt.Printf("log path: %s\n", log.Dump("./log"))
			conclusion = result.TimedOut
			output = fmt.Sprintf("timed out after %v", timeout)
			return
		case err := <-errCh:
			cancel()
			if err != nil {
				conclusion = result.Failure
				output = err.Error()
				fmt.Println(err)
				log.Dump("./log")
				if failfast {
					return
				}
			} else if totalRound == 1 {
				log.Dump("./log")
			}
		}
		if totalRound != 0 && totalRound <= round {
			return
		}
	}
}

type ColumnType struct {
	i    int
	name string
	tp   kv.DataType
	len  int
	null bool
}

func (c *ColumnType) ToColStr() string {
	var b strings.Builder
	if c.len > 0 {
		fmt.Fprintf(&b, "%s %s(%d)", c.name, c.tp, c.len)
	} else {
		fmt.Fprintf(&b, "%s %s", c.name, c.tp)
	}
	if !c.null {
		b.WriteString(" NOT")
	}
	b.WriteString(" NULL")
	return b.String()
}

func NewColumnType(i int, name string, tp kv.DataType, len int, null bool) ColumnType {
	return ColumnType{
		i:    i,
		name: name,
		tp:   tp,
		len:  len,
		null: null,
	}
}

func RdColumnsAndPk(leastCol int) ([]ColumnType, []ColumnType) {
	columns := rdColumns(leastCol)
	columns[0].null = false
	primary := []ColumnType{columns[0]}
	for pi := 1; pi < len(columns) && columns[pi-1].tp == kv.TinyInt || pi <= 2; pi++ {
		columns[pi].null = false
		primary = append(primary, columns[pi])
	}
	return columns, primary
}

func rdColumns(least int) []ColumnType {
	colCnt = util.RdRange(columnLeast, columnMost)
	if colCnt < least {
		colCnt = least
	}
	columns := make([]ColumnType, colCnt)

	for i := 0; i < colCnt; i++ {
		tp := kv.RdType()
		columns[i] = NewColumnType(i, fmt.Sprintf("col_%d", i), tp, tp.Size(), util.RdBool())
	}

	return columns
}

func MustExec(db *sql.DB, dsn string, sqlStmt string, retry bool, dupError string) {
	var err error
	maxtry := 20
	for i := 0; i < maxtry; i++ {
		_, err = db.Exec(sqlStmt)
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "connection refused") && i < maxtry-1 {
			for err != nil {
				time.Sleep(500 * time.Second)
				db, err = sql.Open("mysql", dsn)
			}
			continue
		}
		if dupError != "" && strings.Contains(err.Error(), dupError) {
			return
		}
		if !retry {
			break
		}
	}
	panic(sqlStmt + ": " + err.Error())
}

func GenDropTableStmt(tableName string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s", tableName)
}

func GenCreateTableStmt(columns, primary []ColumnType, tableName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s(\n", tableName)
	for i, column := range columns {
		if column.len > 0 {
			fmt.Fprintf(&b, "%s %s(%d)", column.name, column.tp, column.len)
		} else {
			fmt.Fprintf(&b, "%s %s", column.name, column.tp)
		}
		if !column.null {
			b.WriteString(" NOT")
		}
		b.WriteString(" NULL")
		if i != len(columns)-1 {
			b.WriteString(",\n")
		}
	}

	var indexes []string
	ps := len(primary)
	if ps > 0 {
		columns := make([]string, ps)
		for i := 0; i < ps; i++ {
			columns[i] = primary[i].name
		}
		clustered := ""
		if clusteredIndex {
			clustered = "CLUSTERED"
		}
		indexes = append(indexes, fmt.Sprintf("PRIMARY KEY(%s) %s", strings.Join(columns, ", "), clustered))
	}

	for _, index := range indexes {
		fmt.Fprintf(&b, ",\n%s", index)
	}

	b.WriteString(")")
	return b.String()
}

func once(db, db2 *sql.DB, log *Log) error {
	indexSet = make(map[string]struct{})
	uniqueIndexSet = make(map[string]struct{})
	leastCol := 10
	if txnSize >= int64(200*mbSize) {
		leastCol = 100
	}
	columns, primary := RdColumnsAndPk(leastCol)
	uniqueSets.NewIndex("primary", primary)
	initThreadName := "init"
	clearTableStmt := GenDropTableStmt(tableName)
	createTableStmt := GenCreateTableStmt(columns, primary, tableName)

	util.AssertNil(log.NewThread(initThreadName))

	initLogIndex := log.Exec(initThreadName, clearTableStmt)
	MustExec(db, dsn1, clearTableStmt, true, "")
	log.Done(initThreadName, initLogIndex, nil)

	initLogIndex = log.Exec(initThreadName, createTableStmt)
	if _, err := db.Exec(createTableStmt); err != nil {
		fmt.Println(err)
		uniqueSets.Reset()
		return nil
	}
	log.Done(initThreadName, initLogIndex, nil)

	var (
		wg            sync.WaitGroup
		readyDMLWg    sync.WaitGroup
		readyDDLWg    sync.WaitGroup
		doneInsertWg  sync.WaitGroup
		readyCommitWg sync.WaitGroup
	)
	wg.Add(dmlThread)
	doneInsertWg.Add(dmlThread)
	readyDDLWg.Add(dmlThread)
	readyDMLWg.Add(len(modeFns))
	readyCommitWg.Add(len(modeFns))

	snapshotSchema := CloneColumns(columns)

	for _, fn := range modeFns {
		go fn(&columns, db, log, &readyDMLWg, &readyDDLWg, &readyCommitWg)
	}

	dmlExecutor(&snapshotSchema, db, log, dmlExecutorOption{
		dmlCnt:        dmlCnt,
		dmlThread:     dmlThread,
		readyDMLWg:    &readyDMLWg,
		readyDDLWg:    &readyDDLWg,
		readyCommitWg: &readyCommitWg,
		doneWg:        &wg,
	})

	wg.Wait()

	uniqueSets.Reset()
	if db2 != nil {
		// reconnect to downstream database since it's been re-created
		var err error
		db2, err = sql.Open("mysql", dsn2)
		if err != nil {
			panic(err)
		}
		now := time.Now().Unix()
		MustExec(db, dsn1, fmt.Sprintf("INSERT INTO %s VALUES(%d)", checkTableName, now), true, "")
		fmt.Println("wait for sync")
		check.WaitSync(db2, now, checkTableName)
		fmt.Println("ready to check")
		// since the txn's order between tables is not garuantted, we wait extra 10 seconds
		time.Sleep(10 * time.Second)
	}
	return check.Check(db, db2, tableName)
}

func CloneColumns(source []ColumnType) []ColumnType {
	res := make([]ColumnType, len(source))
	for i := 0; i < len(source); i++ {
		res[i] = ColumnType{
			i:    source[i].i,
			name: source[i].name,
			tp:   source[i].tp,
			len:  source[i].len,
			null: source[i].null,
		}
	}
	return res
}

func CloneColumn(source ColumnType) ColumnType {
	return source
}
