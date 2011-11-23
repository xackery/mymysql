// Thread unsafe engine for MyMySQL
package native

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"reflect"
	"github.com/ziutek/mymysql/mysql"
)

type serverInfo struct {
	prot_ver byte
	serv_ver string
	thr_id   uint32
	scramble []byte
	caps     uint16
	lang     byte
}

// MySQL connection handler
type Conn struct {
	proto string // Network protocol
	laddr string // Local address
	raddr string // Remote (server) address

	user   string // MySQL username
	passwd string // MySQL password
	dbname string // Database name

	net_conn net.Conn // MySQL connection
	rd       *bufio.Reader
	wr       *bufio.Writer

	info serverInfo // MySQL server information
	seq  byte       // MySQL sequence number

	unreaded_rows bool

	init_cmds []string         // MySQL commands/queries executed after connect
	stmt_map  map[uint32]*Stmt // For reprepare during reconnect

	// Current status of MySQL server connection
	status uint16

	// Maximum packet size that client can accept from server.
	// Default 16*1024*1024-1. You may change it before connect.
	max_pkt_size int

	// Debug logging. You may change it at any time.
	Debug bool
}

// Create new MySQL handler. The first three arguments are passed to net.Bind
// for create connection. user and passwd are for authentication. Optional db
// is database name (you may not specifi it and use Use() method later).
func New(proto, laddr, raddr, user, passwd string, db ...string) mysql.Conn {
	my := Conn{
		proto:        proto,
		laddr:        laddr,
		raddr:        raddr,
		user:         user,
		passwd:       passwd,
		stmt_map:     make(map[uint32]*Stmt),
		max_pkt_size: 16*1024*1024 - 1,
	}
	if len(db) == 1 {
		my.dbname = db[0]
	} else if len(db) > 1 {
		panic("mymy.New: too many arguments")
	}
	return &my
}

// If new_size > 0 sets maximum packet size. Returns old size.
func (my *Conn) SetMaxPktSize(new_size int) int {
	old_size := my.max_pkt_size
	if new_size > 0 {
		my.max_pkt_size = new_size
	}
	return old_size
}

func (my *Conn) connect() (err error) {
	defer catchError(&err)

	// Make connection
	switch my.proto {
	case "tcp", "tcp4", "tcp6":
		var la, ra *net.TCPAddr
		if my.laddr != "" {
			if la, err = net.ResolveTCPAddr("", my.laddr); err != nil {
				return
			}
		}
		if my.raddr != "" {
			if ra, err = net.ResolveTCPAddr("", my.raddr); err != nil {
				return
			}
		}
		if my.net_conn, err = net.DialTCP(my.proto, la, ra); err != nil {
			return
		}

	case "unix":
		var la, ra *net.UnixAddr
		if my.raddr != "" {
			if ra, err = net.ResolveUnixAddr(my.proto, my.raddr); err != nil {
				return
			}
		}
		if my.laddr != "" {
			if la, err = net.ResolveUnixAddr(my.proto, my.laddr); err != nil {
				return
			}
		}
		if my.net_conn, err = net.DialUnix(my.proto, la, ra); err != nil {
			return
		}

	default:
		err = net.UnknownNetworkError(my.proto)
	}

	my.rd = bufio.NewReader(my.net_conn)
	my.wr = bufio.NewWriter(my.net_conn)

	// Initialisation
	my.init()
	my.auth()
	my.getResult(nil)

	// Execute all registered commands
	for _, cmd := range my.init_cmds {
		// Send command
		my.sendCmd(_COM_QUERY, cmd)
		// Get command response
		res := my.getResponse()

		if res.field_count == 0 {
			// No fields in result (OK result)
			continue
		}
		// Read and discard all result rows
		var row mysql.Row
		for {
			row, err = res.getRow()
			if err != nil {
				return
			}
			if row == nil {
				res, err = res.nextResult()
				if err != nil {
					return
				}
				if res == nil {
					// No more rows and results from this cmd
					break
				}
			}
		}
	}

	return
}

// Establishes a connection with MySQL server version 4.1 or later.
func (my *Conn) Connect() (err error) {
	if my.net_conn != nil {
		return ALREDY_CONN_ERROR
	}

	return my.connect()
}

// Check if connection is established
func (my *Conn) IsConnected() bool {
	return my.net_conn != nil
}

func (my *Conn) closeConn() (err error) {
	defer catchError(&err)

	// Always close and invalidate connection, even if
	// COM_QUIT returns an error
	defer func() {
		err = my.net_conn.Close()
		my.net_conn = nil // Mark that we disconnect
	}()

	// Close the connection
	my.sendCmd(_COM_QUIT)
	return
}

// Close connection to the server
func (my *Conn) Close() (err error) {
	if my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}

	return my.closeConn()
}

// Close and reopen connection.
// Ignore unreaded rows, reprepare all prepared statements.
func (my *Conn) Reconnect() (err error) {
	if my.net_conn != nil {
		// Close connection, ignore all errors
		my.closeConn()
	}
	// Reopen the connection.
	if err = my.connect(); err != nil {
		return
	}

	// Reprepare all prepared statements
	var (
		new_stmt *Stmt
		new_map  = make(map[uint32]*Stmt)
	)
	for _, stmt := range my.stmt_map {
		new_stmt, err = my.prepare(stmt.sql)
		if err != nil {
			return
		}
		// Assume that fields set in new_stmt by prepare() are indentical to
		// corresponding fields in stmt. Why can they be different?
		stmt.id = new_stmt.id
		stmt.rebind = true
		new_map[stmt.id] = stmt
	}
	// Replace the stmt_map
	my.stmt_map = new_map

	return
}

// Change database
func (my *Conn) Use(dbname string) (err error) {
	defer catchError(&err)

	if my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}

	// Send command
	my.sendCmd(_COM_INIT_DB, dbname)
	// Get server response
	my.getResult(nil)
	// Save new database name if no errors
	my.dbname = dbname

	return
}

func (my *Conn) getResponse() (res *Result) {
	res, ok := my.getResult(nil).(*Result)
	if !ok {
		panic(BAD_RESULT_ERROR)
	}
	if res.field_count != 0 {
		// This query can return rows (this isn't OK result)
		my.unreaded_rows = true
	}
	return
}

// Start new query.
//
// If you specify the parameters, the SQL string will be a result of
// fmt.Sprintf(sql, params...).
// You must get all result rows (if they exists) before next query.
func (my *Conn) Start(sql string, params ...interface{}) (res mysql.Result, err error) {
	defer catchError(&err)

	if my.net_conn == nil {
		return nil, NOT_CONN_ERROR
	}
	if my.unreaded_rows {
		return nil, UNREADED_ROWS_ERROR
	}

	if len(params) != 0 {
		sql = fmt.Sprintf(sql, params...)
	}
	// Send query
	my.sendCmd(_COM_QUERY, sql)

	// Get command response
	res = my.getResponse()
	return
}

func (res *Result) getRow() (row mysql.Row, err error) {
	defer catchError(&err)

	switch result := res.my.getResult(res).(type) {
	case mysql.Row:
		// Row of data
		row = result

	case *Result:
		// EOF result

	default:
		err = BAD_RESULT_ERROR
	}
	return
}

func (res *Result) MoreResults() bool {
	return res.status&_SERVER_MORE_RESULTS_EXISTS != 0
}

// Get the data row from server. This method reads one row of result directly
// from network connection (without rows buffering on client side).
func (res *Result) GetRow() (row mysql.Row, err error) {
	if res.field_count == 0 {
		// There is no fields in result (OK result)
		return
	}
	row, err = res.getRow()
	if err == nil && row == nil && !res.MoreResults() {
		res.my.unreaded_rows = false
	}
	return
}

func (res *Result) nextResult() (next *Result, err error) {
	defer catchError(&err)
	if res.MoreResults() {
		next = res.my.getResponse()
	}
	return
}

// This function is used when last query was the multi result query.
// Return the next result or nil if no more resuts exists.
func (res *Result) NextResult() (mysql.Result, error) {
	res, err := res.nextResult()
	return res, err
}

// Send MySQL PING to the server.
func (my *Conn) Ping() (err error) {
	defer catchError(&err)

	if my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}

	// Send command
	my.sendCmd(_COM_PING)
	// Get server response
	my.getResult(nil)

	return
}

func (my *Conn) prepare(sql string) (stmt *Stmt, err error) {
	defer catchError(&err)

	// Send command
	my.sendCmd(_COM_STMT_PREPARE, sql)
	// Get server response
	stmt, ok := my.getPrepareResult(nil).(*Stmt)
	if !ok {
		return nil, BAD_RESULT_ERROR
	}
	if len(stmt.params) > 0 {
		// Get param fields
		my.getPrepareResult(stmt)
	}
	if len(stmt.fields) > 0 {
		// Get column fields
		my.getPrepareResult(stmt)
	}
	return
}

// Prepare server side statement. Return statement handler.
func (my *Conn) Prepare(sql string) (mysql.Stmt, error) {
	if my.net_conn == nil {
		return nil, NOT_CONN_ERROR
	}
	if my.unreaded_rows {
		return nil, UNREADED_ROWS_ERROR
	}

	stmt, err := my.prepare(sql)
	if err != nil {
		return nil, err
	}
	// Connect statement with database handler
	my.stmt_map[stmt.id] = stmt
	// Save SQL for reconnect
	stmt.sql = sql

	return stmt, nil
}

// Bind input data for the parameter markers in the SQL statement that was
// passed to Prepare.
// 
// params may be a parameter list (slice), a struct or a pointer to the struct.
// A struct field can by value or pointer to value. A parameter (slice element)
// can be value, pointer to value or pointer to pointer to value.
// Values may be of the folowind types: intXX, uintXX, floatXX, []byte, Blob,
// string, Datetime, Date, Time, Timestamp, Raw.
func (stmt *Stmt) BindParams(params ...interface{}) {
	stmt.rebind = true

	// Check for struct binding
	if len(params) == 1 {
		pval := reflect.ValueOf(params[0])
		kind := pval.Kind()
		if kind == reflect.Ptr {
			// Dereference pointer
			pval = pval.Elem()
			kind = pval.Kind()
		}
		typ := pval.Type()
		if kind == reflect.Struct &&
			typ != mysql.DatetimeType &&
			typ != mysql.DateType &&
			typ != mysql.TimestampType &&
			typ != mysql.RawType {
			// We have struct to bind
			if pval.NumField() != stmt.param_count {
				panic(BIND_COUNT_ERROR)
			}
			if !pval.CanAddr() {
				// Make an addressable structure
				v := reflect.New(pval.Type()).Elem()
				v.Set(pval)
				pval = v
			}
			for ii := 0; ii < stmt.param_count; ii++ {
				stmt.params[ii] = bindValue(pval.Field(ii))
			}
			return
		}

	}

	// There isn't struct to bind

	if len(params) != stmt.param_count {
		panic(BIND_COUNT_ERROR)
	}
	for ii, par := range params {
		pval := reflect.ValueOf(par)
		if pval.IsValid() {
			if pval.Kind() == reflect.Ptr {
				// Dereference pointer - this value i addressable
				pval = pval.Elem()
			} else {
				// Make an addressable value
				v := reflect.New(pval.Type()).Elem()
				v.Set(pval)
				pval = v
			}
		}
		stmt.params[ii] = bindValue(pval)
	}
}

// Resets the previous parameter binding
func (stmt *Stmt) ResetParams() {
	stmt.rebind = true
	for ii := 0; ii < stmt.param_count; ii++ {
		stmt.params[ii] = nil
	}
}

// Execute prepared statement. If statement requires parameters you may bind
// them first or specify directly. After this command you may use GetRow to
// retrieve data.
func (stmt *Stmt) Run(params ...interface{}) (res mysql.Result, err error) {
	defer catchError(&err)

	if stmt.my.net_conn == nil {
		return nil, NOT_CONN_ERROR
	}
	if stmt.my.unreaded_rows {
		return nil, UNREADED_ROWS_ERROR
	}

	// Bind parameters if any
	if len(params) != 0 {
		stmt.BindParams(params...)
	} else if len(stmt.params) != stmt.param_count {
		panic(BIND_COUNT_ERROR)
	}

	// Send EXEC command with binded parameters
	stmt.sendCmdExec()
	// Get response
	r := stmt.my.getResponse()
	r.binary = true
	res = r
	return
}

// Destroy statement on server side. Client side handler is invalid after this
// command.
func (stmt *Stmt) Delete() (err error) {
	defer catchError(&err)

	if stmt.my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if stmt.my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}

	// Allways delete statement on client side, even if
	// the command return an error.
	defer func() {
		// Delete statement from stmt_map
		delete(stmt.my.stmt_map, stmt.id)
		// Invalidate handler
		*stmt = Stmt{}
	}()

	// Send command
	stmt.my.sendCmd(_COM_STMT_CLOSE, stmt.id)
	return
}

// Resets a prepared statement on server: data sent to the server, unbuffered
// result sets and current errors.
func (stmt *Stmt) Reset() (err error) {
	defer catchError(&err)

	if stmt.my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if stmt.my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}

	// Next exec must send type information. We set rebind flag regardless of
	// whether the command succeeds or not.
	stmt.rebind = true
	// Send command
	stmt.my.sendCmd(_COM_STMT_RESET, stmt.id)
	// Get result
	stmt.my.getResult(nil)
	return
}

// Send long data to MySQL server in chunks.
// You can call this method after Bind and before Exec. It can be called
// multiple times for one parameter to send TEXT or BLOB data in chunks.
//
// pnum     - Parameter number to associate the data with.
//
// data     - Data source string, []byte or io.Reader.
//
// pkt_size - It must be must be greater than 6 and less or equal to MySQL
// max_allowed_packet variable. You can obtain value of this variable
// using such query: SHOW variables WHERE Variable_name = 'max_allowed_packet'
// If data source is io.Reader then (pkt_size - 6) is size of a buffer that
// will be allocated for reading. 
//
// If you have data source of type string or []byte in one piece you may
// properly set pkt_size and call this method once. If you have data in
// multiple pieces you can call this method multiple times. If data source is
// io.Reader you should properly set pkt_size. Data will be readed from
// io.Reader and send in pieces to the server until EOF.
func (stmt *Stmt) SendLongData(pnum int, data interface{}, pkt_size int) (err error) {
	defer catchError(&err)

	if stmt.my.net_conn == nil {
		return NOT_CONN_ERROR
	}
	if stmt.my.unreaded_rows {
		return UNREADED_ROWS_ERROR
	}
	if pnum < 0 || pnum >= stmt.param_count {
		return WRONG_PARAM_NUM_ERROR
	}
	if pkt_size -= 6; pkt_size < 0 {
		return SMALL_PKT_SIZE_ERROR
	}

	switch dd := data.(type) {
	case io.Reader:
		buf := make([]byte, pkt_size)
		for {
			nn, ee := io.ReadFull(dd, buf)
			if ee == io.EOF {
				return
			}
			if nn != 0 {
				stmt.my.sendCmd(
					_COM_STMT_SEND_LONG_DATA,
					stmt.id, uint16(pnum), buf[0:nn],
				)
			}
			if ee == io.ErrUnexpectedEOF {
				return
			} else if ee != nil {
				return ee
			}
		}

	case []byte:
		for len(dd) > pkt_size {
			stmt.my.sendCmd(
				_COM_STMT_SEND_LONG_DATA,
				stmt.id, uint16(pnum), dd[0:pkt_size],
			)
			dd = dd[pkt_size:]
		}
		stmt.my.sendCmd(_COM_STMT_SEND_LONG_DATA, stmt.id, uint16(pnum), dd)
		return

	case string:
		for len(dd) > pkt_size {
			stmt.my.sendCmd(
				_COM_STMT_SEND_LONG_DATA,
				stmt.id, uint16(pnum), dd[0:pkt_size],
			)
			dd = dd[pkt_size:]
		}
		stmt.my.sendCmd(_COM_STMT_SEND_LONG_DATA, stmt.id, uint16(pnum), dd)
		return
	}
	return UNK_DATA_TYPE_ERROR
}

// Returns the thread ID of the current connection.
func (my *Conn) ThreadId() uint32 {
	return my.info.thr_id
}

// Register MySQL command/query to be executed immediately after connecting to
// the server. You may register multiple commands. They will be executed in
// the order of registration. Yhis method is mainly useful for reconnect.
func (my *Conn) Register(sql string) {
	my.init_cmds = append(my.init_cmds, sql)
}

// See mysql.Query
func (my *Conn) Query(sql string, params ...interface{}) ([]mysql.Row, mysql.Result, error){
	return mysql.Query(my, sql, params...)
}

// See mysql.Exec
func (stmt *Stmt) Exec(params ...interface{}) ([]mysql.Row, mysql.Result, error) {
	return mysql.Exec(stmt, params...)
}

// See mysql.End
func (res *Result) End() error {
	return mysql.End(res)
}

// See mysql.GetRows
func (res *Result) GetRows() ([]mysql.Row, error) {
	return mysql.GetRows(res)
}

// Escapes special characters in the txt, so it is safe to place returned string
// to Query method.
func (my *Conn) EscapeString(txt string) string {
	if my.status&_SERVER_STATUS_NO_BACKSLASH_ESCAPES != 0 {
		return escapeQuotes(txt)
	}
	return escapeString(txt)
}

type Transaction struct {
	*Conn
}

// Starts a new transaction
func (my *Conn) Begin() (mysql.Transaction, error) {
	_, err := my.Start("START TRANSACTION")
	return &Transaction{my}, err
}

// Commit a transaction
func (tr Transaction) Commit() error {
	_, err := tr.Start("COMMIT")
	tr.Conn = nil // Invalidate this transaction
	return err
}

// Rollback a transaction
func (tr Transaction) Rollback() error {
	_, err := tr.Start("ROLLBACK")
	tr.Conn = nil // Invalidate this transaction
	return err
}

// Binds statement to the context of transaction. For native engine this is
// identity function.
func (tr Transaction) Do(st mysql.Stmt) mysql.Stmt {
	if s, ok := st.(*Stmt); !ok || s.my != tr.Conn {
		panic("Transaction and statement doesn't belong to the same connection")
	}
	return st
}

func init() {
	mysql.New = New
}
