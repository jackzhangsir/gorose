package gorose

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

type Session struct {
	IEngin
	*Binder
	slaveDB      *sql.DB
	masterDB     *sql.DB
	tx           *sql.Tx
	masterDriver string
	slaveDriver  string
	lastInsertId int64
	sqlLogs      []string
	lastSql      string
}

var _ ISession = &Session{}

// NewSession : 初始化 Session
func NewSession(e IEngin) ISession {
	var s = new(Session)
	s.IEngin = e

	s.masterDB,s.masterDriver = s.IEngin.GetExecuteDB()
	s.slaveDB,s.slaveDriver = s.IEngin.GetQueryDB()

	s.Binder = new(Binder)

	return s
}

// Close : 关闭 Session
func (s *Session) Close() {
	s.masterDB.Close()
	s.slaveDB.Close()
}

// GetDriver 获取驱动
func (s *Session) GetDriver() string {
	return s.masterDriver
}

// Table : the mirror of Bind()
func (s *Session) Table(tab interface{}) ISession {
	return s.Bind(tab)
}

// Bind : 传入绑定结果的对象, 参数一为对象, 可以是 struct, gorose.MapRow 或对应的切片
//		如果是做非query操作,第一个参数也可以仅仅指定为字符串表名
func (s *Session) Bind(tab interface{}) ISession {
	s.BindOrigin = tab
	_ = s.parseTable()

	return s
}

func (s *Session) Begin() (err error) {
	s.tx, err = s.masterDB.Begin()
	return
}

func (s *Session) Rollback() (err error) {
	err = s.tx.Rollback()
	s.tx = nil
	return
}

func (s *Session) Commit() (err error) {
	err = s.tx.Commit()
	s.tx = nil
	return
}

func (s *Session) Transaction(closers ...func(ses ISession) error) (err error) {
	err = s.Begin()
	if err != nil {
		return err
	}

	for _, closer := range closers {
		err = closer(s)
		if err != nil {
			_ = s.Rollback()
			return
		}
	}
	return s.Commit()
}

func (s *Session) Query(sqlstring string, args ...interface{}) error {
	s.lastSql = fmt.Sprintf(sqlstring, args...)
	// 记录sqlLog
	if s.IfEnableQueryLog() {
		s.sqlLogs = append(s.sqlLogs, s.lastSql)
	}

	stmt, err := s.slaveDB.Prepare(sqlstring)
	if err != nil {
		return err
	}

	defer stmt.Close()
	rows, err := stmt.Query(args...)
	if err != nil {
		return err
	}

	// make sure we always close rows
	defer rows.Close()

	return s.scan(rows)
}

func (s *Session) Execute(sqlstring string, args ...interface{}) (rowsAffected int64, err error) {
	//t_start := time.Now()

	s.lastSql = fmt.Sprintf(sqlstring, args...)
	// 记录sqlLog
	if s.IfEnableQueryLog() {
		s.sqlLogs = append(s.sqlLogs, s.lastSql)
	}

	var operType = strings.ToLower(sqlstring[0:6])
	if operType == "select" {
		return 0, errors.New("Execute does not allow select operations, please use Query")
	}

	var stmt *sql.Stmt
	if s.tx == nil {
		stmt, err = s.masterDB.Prepare(sqlstring)
	} else {
		stmt, err = s.tx.Prepare(sqlstring)
	}

	if err != nil {
		return 0, err
	}
	//return dba.parseExecute(stmt, operType, vals)

	//var err error
	defer stmt.Close()
	result, errs := stmt.Exec(args...)
	if errs != nil {
		return 0, errs
	}

	if operType == "insert" {
		// get last insert id
		lastInsertId, err := result.LastInsertId()
		if err == nil {
			s.lastInsertId = lastInsertId
		}
	}
	// get rows affected
	rowsAffected, err = result.RowsAffected()

	//// 如果是事务, 则重置所有参数
	//if dba.Strans == true {
	//	dba.Reset("transaction")
	//}

	//// 持久化日志
	//if dba.Connection.Logger != nil {
	//	dba.Connection.Logger.Write(dba.lastSql, time.Since(t_start).String(), time.Now().Format("2006-01-02 15:04:05"))
	//}

	return rowsAffected, err
}
func (s *Session) LastInsertId() int64 {
	return s.lastInsertId
}
func (s *Session) LastInsertSql() string {
	return s.lastSql
}

func (s *Session) scan(rows *sql.Rows) (err error) {
	//fmt.Printf("%#v\n",s.table)
	// 检查实多维数组还是一维数组
	switch s.BindType {
	case OBJECT_STRUCT:
		err = s.scanRow(rows, s.BindOrigin)
	case OBJECT_STRUCT_SLICE:
		err = s.scanAll(rows, s.BindResultSlice)
	case OBJECT_MAP:
		err = s.scanMap(rows, s.BindResult)
	case OBJECT_MAP_SLICE:
		err = s.scanMapAll(rows, s.BindResultSlice)
	default:
		err = errors.New("Bind value error")
	}
	return
}

func (s *Session) scanMap(rows *sql.Rows, dst interface{}) (err error) {
	return s.scanMapAll(rows, dst)
}

func (s *Session) scanMapAll(rows *sql.Rows, dst interface{}) (err error) {
	//var result = make([]map[string]interface{}, 0)
	var columns []string
	if columns, err = rows.Columns(); err != nil {
		return
	}

	count := len(columns)
	values := make([]interface{}, count)
	scanArgs := make([]interface{}, count)

	for rows.Next() {
		for i := 0; i < count; i++ {
			scanArgs[i] = &values[i]
		}
		rows.Scan(scanArgs...)
		//entry := make(map[string]interface{})
		for i, col := range columns {
			var v interface{}
			val := values[i]
			if b, ok := val.([]byte); ok {
				v = string(b)
			} else {
				v = val
			}
			//entry[col] = v
			s.BindResult.SetMapIndex(reflect.ValueOf(col), reflect.ValueOf(v))
		}
		//result = append(result, entry)
		if s.BindType == OBJECT_MAP_SLICE {
			s.BindResultSlice.Set(reflect.Append(s.BindResultSlice, s.BindResult))
		}
	}
	return
}

// scan a single row of data into a struct.
func (s *Session) scanRow(rows *sql.Rows, dst interface{}) error {
	// check if there is data waiting
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}

	// get a list of targets
	var fields = strutForScan(dst)

	// perform the scan
	if err := rows.Scan(fields...); err != nil {
		//if err := rows.Scan(strutForScan(s.BindResult.Interface())...); err != nil {
		return err
	}

	return rows.Err()
}

// ScanAll scans all sql result rows into a slice of structs.
// It reads all rows and closes rows when finished.
// dst should be a pointer to a slice of the appropriate type.
// The new results will be appended to any existing data in dst.
func (s *Session) scanAll(rows *sql.Rows, dst interface{}) error {
	for rows.Next() {
		// scan it
		err := rows.Scan(strutForScan(s.BindResult.Interface())...)
		if err != nil {
			return err
		}
		// add to the result slice
		s.BindResultSlice.Set(reflect.Append(s.BindResultSlice, s.BindResult.Elem()))
	}

	return rows.Err()
}

func (s *Session) parseTable() (err error) {
	if s.BindOrigin == nil {
		return nil
	}
	var BindName string
	switch s.BindOrigin.(type) {
	case string: // 直接传入的是表名
		s.BindType = OBJECT_STRING
		BindName = s.BindOrigin.(string)

	// 传入的是struct
	default:
		// 清空字段值,避免手动传入字段污染struct字段
		s.BindFields = []string{}
		// make sure dst is an appropriate type
		dstVal := reflect.ValueOf(s.BindOrigin)

		sliceVal := reflect.Indirect(dstVal)

		switch sliceVal.Kind() {
		case reflect.Struct: // struct
			s.BindType = OBJECT_STRUCT
			BindName = sliceVal.Type().Name()
			s.BindResult = sliceVal
			// 默认只查一条
			s.BindLimit = 1
			// 是否设置了表名
			if tn := dstVal.MethodByName("BindName"); tn.IsValid() {
				BindName = tn.Call(nil)[0].String()
			}
			// 解析出字段
			s.parseFields()
		case reflect.Map: // map
			//fmt.Println("map")
			s.BindType = OBJECT_MAP
			// 默认只查一条
			s.BindLimit = 1
			//
			s.BindResult = sliceVal

		case reflect.Slice: // []struct
			eltType := sliceVal.Type().Elem()

			switch eltType.Kind() {
			case reflect.Map:
				s.BindType = OBJECT_MAP_SLICE
				//BindName = eltType.Name()
				s.BindResult = reflect.MakeMap(eltType)
				s.BindResultSlice = sliceVal

			case reflect.Struct:
				s.BindType = OBJECT_STRUCT_SLICE
				BindName = eltType.Name()
				s.BindResult = reflect.New(eltType)
				s.BindResultSlice = sliceVal
				// 是否设置了表名
				if tn := s.BindResult.MethodByName("BindName"); tn.IsValid() {
					BindName = tn.Call(nil)[0].String()
				}
				// 解析出字段
				s.parseFields()
			default:
				return fmt.Errorf("table只接收 struct,[]struct,map[string]interface{},[]map[string]interface{}, 但是传入的是: %T", s.BindOrigin)
			}
		default:
			return fmt.Errorf("table只接收 struct,[]struct,map[string]interface{},[]map[string]interface{}, 但是传入的是: %T", s.BindOrigin)
		}
	}

	s.BindName = BindName

	return
}

func (s *Session) parseFields() {
	if len(s.BindFields) == 0 {
		s.BindFields = getTagName(s.BindResult.Interface())
	}
}
