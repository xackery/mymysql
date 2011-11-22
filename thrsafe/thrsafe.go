// MySQL Client API written entirely in Go without any external dependences.
// This is thread safe wrapper over native engine.
// See documentation of mymysql/native for details/
package thrsafe

import (
	"sync"
	//"log"
	"github.com/ziutek/mymysql/mysql"
	_ "github.com/ziutek/mymysql/native"
)

type Conn struct {
	mysql.Conn
	mutex *sync.Mutex
}

func (c *Conn) lock() {
	//log.Println(c, ":: lock @", c.mutex)
	c.mutex.Lock()
}

func (c *Conn) unlock() {
	//log.Println(c, ":: unlock @", c.mutex)
	c.mutex.Unlock()
}

type Result struct {
	mysql.Result
	conn *Conn
}

type Stmt struct {
	mysql.Stmt
	conn *Conn
}

type Transaction struct {
	*Conn
	conn *Conn
}

func New(proto, laddr, raddr, user, passwd string, db ...string) mysql.Conn {
	return &Conn{
		orgNew(proto, laddr, raddr, user, passwd, db...),
		new(sync.Mutex),
	}
}

func (c *Conn) Connect() error {
	c.lock()
	defer c.unlock()
	return c.Conn.Connect()
}

func (c *Conn) Close() error {
	c.lock()
	defer c.unlock()
	return c.Conn.Close()
}

func (c *Conn) Reconnect() error {
	c.lock()
	defer c.unlock()
	return c.Conn.Reconnect()
}

func (c *Conn) Use(dbname string) error {
	c.lock()
	defer c.unlock()
	return c.Conn.Use(dbname)
}

func (c *Conn) Start(sql string, params ...interface{}) (mysql.Result, error) {
	c.lock()
	res, err := c.Conn.Start(sql, params...)
	// Unlock if error or OK result (which doesn't provide any fields)
	if err != nil {
		c.unlock()
		return nil, err
	}
	if len(res.Fields()) == 0 {
		c.unlock()
	}
	return &Result{res, c}, err
}

func (res *Result) GetRow() (mysql.Row, error) {
	row, err := res.Result.GetRow()
	if err != nil || row == nil && !res.MoreResults() {
		res.conn.unlock()
	}
	return row, err
}

func (res *Result) NextResult() (mysql.Result, error) {
	next, err := res.Result.NextResult()
	if err != nil {
		return nil, err
	}
	return &Result{next, res.conn}, nil
}

func (c *Conn) Ping() error {
	c.lock()
	defer c.unlock()
	return c.Conn.Ping()
}

func (c *Conn) Prepare(sql string) (mysql.Stmt, error) {
	c.lock()
	defer c.unlock()
	stmt, err := c.Conn.Prepare(sql)
	if err != nil {
		return nil, err
	}
	return &Stmt{stmt, c}, nil
}

func (stmt *Stmt) Run(params ...interface{}) (mysql.Result, error) {
	stmt.conn.lock()
	res, err := stmt.Stmt.Run(params...)
	// Unlock if error or OK result (which doesn't provide any fields)
	if err != nil {
		stmt.conn.unlock()
		return nil, err
	}
	if len(res.Fields()) == 0 {
		stmt.conn.unlock()
	}
	return &Result{res, stmt.conn}, nil
}

func (stmt *Stmt) Delete() error {
	stmt.conn.lock()
	defer stmt.conn.unlock()
	return stmt.Stmt.Delete()
}

func (stmt *Stmt) Reset() error {
	stmt.conn.lock()
	defer stmt.conn.unlock()

	return stmt.Stmt.Delete()
}

func (stmt *Stmt) SendLongData(pnum int, data interface{}, pkt_size int) error {
	stmt.conn.lock()
	defer stmt.conn.unlock()
	return stmt.Stmt.SendLongData(pnum, data, pkt_size)
}

// Begins a new transaction. No any other thread can send command on this
// connection until Commit or Rollback will be called.
func (c *Conn) Begin() (mysql.Transaction, error) {
	c.lock()
	tr := Transaction{
		&Conn{c.Conn, new(sync.Mutex)},
		c,
	}
	_, err := c.Conn.Start("START TRANSACTION")
	if err != nil {
		c.unlock()
		return nil, err
	}
	return &tr, nil
}

func (tr *Transaction) end(cr string) error {
	tr.lock()
	_, err := tr.conn.Conn.Start(cr)
	tr.conn.unlock()
	// Invalidate this transaction
	m := tr.Conn.mutex
	tr.Conn = nil
	tr.conn = nil
	m.Unlock() // One goorutine which still uses this transaction will panic
	return err
}

func (tr *Transaction) Commit() error {
	return tr.end("COMMIT")
}

func (tr *Transaction) Rollback() error {
	return tr.end("ROLLBACK")
}

func (tr *Transaction) Do(st mysql.Stmt) mysql.Stmt {
	if s, ok := st.(*Stmt); ok && s.conn == tr.conn {
		// Returns new statement which uses statement mutexes
		return &Stmt{s.Stmt, tr.Conn}
	}
	panic("Transaction and statement doesn't belong to the same connection")
}

var orgNew func(proto, laddr, raddr, user, passwd string, db ...string) mysql.Conn

func init() {
	orgNew = mysql.New
	mysql.New = New
}