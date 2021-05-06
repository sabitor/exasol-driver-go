package exasol

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os/user"
	"runtime"
	"strconv"

	"github.com/gorilla/websocket"
)

var (
	defaultDialer = *websocket.DefaultDialer
)

type connection struct {
	Config    *Config
	SessionID int
	Metadata  *AuthResponse
	ws        *websocket.Conn
	ctx       context.Context
	isClosed  bool
}

func (c *connection) Exec(query string, args []driver.Value) (driver.Result, error) {
	return c.exec(query, args)
}

func (c *connection) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.query(query, args)
}

func (c *connection) Prepare(query string) (driver.Stmt, error) {
	log.Printf("Prepare")
	if c.isClosed {
		errorLogger.Print(ErrClosed)
		return nil, driver.ErrBadConn
	}

	result := &CreatePreparedStatementResponse{}

	err := c.send(&CreatePreparedStatementCommand{
		Command: Command{"createPreparedStatement"},
		SQLText: query,
	}, result)
	if err != nil {
		return nil, err
	}

	return &statement{
		connection:      c,
		statementHandle: result.StatementHandle,
		numInput:        result.ParameterData.NumColumns,
		columns:         result.ParameterData.Columns,
	}, nil

}

func (c *connection) Close() error {
	return c.close()
}

func (c *connection) Begin() (driver.Tx, error) {
	if c.isClosed {
		errorLogger.Print(ErrClosed)
		return nil, driver.ErrBadConn
	}
	if c.Config.Autocommit {
		return nil, ErrAutocommitEnabled
	}
	return &transaction{
		connection: c,
	}, nil
}

func (c *connection) query(query string, args []driver.Value) (driver.Rows, error) {
	if c.isClosed {
		errorLogger.Print(ErrClosed)
		return nil, driver.ErrBadConn
	}

	// No values provided, simple execute is enough
	if len(args) == 0 {
		result, err := c.simpleExec(query)
		if err != nil {
			return nil, err
		}
		return toRow(result)
	}

	prepResponse := &CreatePreparedStatementResponse{}

	err := c.send(&CreatePreparedStatementCommand{
		Command: Command{"createPreparedStatement"},
		SQLText: query,
	}, prepResponse)
	if err != nil {
		return nil, err
	}

	result, err := c.executePreparedStatement(prepResponse, args)
	if err != nil {
		return nil, err
	}
	return toRow(result)
}

func (c *connection) executePreparedStatement(s *CreatePreparedStatementResponse, args []driver.Value) (*SQLQueriesResponse, error) {
	log.Println("executePreparedStatement")
	columns := s.ParameterData.Columns
	if len(args)%len(columns) != 0 {
		return nil, ErrInvalidValuesCount
	}

	data := make([][]interface{}, len(columns))
	for i, arg := range args {
		if data[i%len(columns)] == nil {
			data[i%len(columns)] = make([]interface{}, 0)
		}
		data[i%len(columns)] = append(data[i%len(columns)], arg)
	}

	command := &ExecutePreparedStatementCommand{
		Command:         Command{"executePreparedStatement"},
		StatementHandle: s.StatementHandle,
		Columns:         columns,
		NumColumns:      len(columns),
		NumRows:         len(data[0]),
		Data:            data,
		/*	Attributes: Attributes{
			ResultSetMaxRows: c.Config.ResultSetMaxRows,
		},*/
	}
	result := &SQLQueriesResponse{}
	err := c.send(command, result)
	if err != nil {
		return nil, err
	}
	if result.NumResults == 0 {
		return nil, ErrMalformData
	}

	return result, c.closePreparedStatement(s)
}

func (c *connection) closePreparedStatement(s *CreatePreparedStatementResponse) error {
	return c.send(&ClosePreparedStatementCommand{
		Command:         Command{"closePreparedStatement"},
		StatementHandle: s.StatementHandle,
	}, nil)
}

func (c *connection) exec(query string, args []driver.Value) (driver.Result, error) {
	if c.isClosed {
		errorLogger.Print(ErrClosed)
		return nil, driver.ErrBadConn
	}

	// No values provided, simple execute is enough
	if len(args) == 0 {
		result, err := c.simpleExec(query)
		if err != nil {
			return nil, err
		}
		return toResult(result)
	}

	prepResponse := &CreatePreparedStatementResponse{}

	err := c.send(&CreatePreparedStatementCommand{
		Command: Command{"createPreparedStatement"},
		SQLText: query,
	}, prepResponse)
	if err != nil {
		return nil, err
	}

	result, err := c.executePreparedStatement(prepResponse, args)
	if err != nil {
		return nil, err
	}
	return toResult(result)
}

func (c *connection) simpleExec(query string) (*SQLQueriesResponse, error) {
	command := &SQLCommand{
		Command: Command{"execute"},
		SQLText: query,
		/*		Attributes: Attributes{
				ResultSetMaxRows: c.Config.ResultSetMaxRows,
			},*/
	}
	result := &SQLQueriesResponse{}
	err := c.send(command, result)
	if err != nil {
		return nil, err
	}
	if result.NumResults == 0 {
		return nil, ErrMalformData
	}
	return result, err
}

func (c *connection) close() error {
	c.isClosed = true
	err := c.send(&Command{Command: "disconnect"}, nil)
	c.ws.Close()
	c.ws = nil
	return err
}

func (c *connection) login() error {
	loginCommand := &LoginCommand{
		Command:         Command{"login"},
		ProtocolVersion: c.Config.ApiVersion,
		Attributes: Attributes{
			Autocommit: c.Config.Autocommit,
		},
	}
	loginResponse := &PublicKeyResponse{}
	err := c.send(loginCommand, loginResponse)
	if err != nil {
		return err
	}

	pubKeyMod, _ := hex.DecodeString(loginResponse.PublicKeyModulus)
	var modulus big.Int
	modulus.SetBytes(pubKeyMod)

	pubKeyExp, _ := strconv.ParseUint(loginResponse.PublicKeyExponent, 16, 32)

	pubKey := rsa.PublicKey{
		N: &modulus,
		E: int(pubKeyExp),
	}
	password := []byte(c.Config.Password)
	encPass, err := rsa.EncryptPKCS1v15(rand.Reader, &pubKey, password)
	if err != nil {
		errorLogger.Printf("password encryption error: %s", err)
		return driver.ErrBadConn
	}
	b64Pass := base64.StdEncoding.EncodeToString(encPass)

	authRequest := AuthCommand{

		Username:       c.Config.User,
		Password:       b64Pass,
		UseCompression: false,
		ClientName:     c.Config.ClientName,
		DriverName:     fmt.Sprintf("go-exasol-client %s", driverVersion),
		ClientOs:       runtime.GOOS,
		ClientVersion:  c.Config.ClientName,
		ClientRuntime:  runtime.Version(),
		Attributes: Attributes{
			Autocommit: c.Config.Autocommit,
		},
	}

	if osUser, err := user.Current(); err != nil {
		authRequest.ClientOsUsername = osUser.Username
	}

	authResponse := &AuthResponse{}
	err = c.send(authRequest, authResponse)
	if err != nil {
		return err
	}
	c.SessionID = authResponse.SessionID
	c.Metadata = authResponse
	c.isClosed = false

	return nil
}