package go_cosmos

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reValNull        = regexp.MustCompile(`(?i)(null)\s*,?`)
	reValNumber      = regexp.MustCompile(`([\d\.xe+-]+)\s*,?`)
	reValBoolean     = regexp.MustCompile(`(?i)(true|false)\s*,?`)
	reValString      = regexp.MustCompile(`(?i)("(\\"|[^"])*?")\s*,?`)
	reValPlaceholder = regexp.MustCompile(`(?i)[$@:](\d+)\s*,?`)
)

type placeholder struct {
	index int
}

// StmtInsert implements "INSERT" operation.
//
// Syntax: INSERT|UPSERT INTO <db-name>.<collection-name> (<field-list>) VALUES (<value-list>)
//
// - values are comma separated.
// - a value is either:
//   - a placeholder (e.g. :1, @2 or $3)
//   - a null
//   - a number
//   - a boolean (true/false)
//   - a string (inside double quotes) that must be a valid JSON, e.g.
//     - a string value in JSON (include the double quotes): "\"a string\""
//     - a number value in JSON (include the double quotes): "123"
//     - a boolean value in JSON (include the double quotes): "true"
//     - a null value in JSON (include the double quotes): "null"
//     - a map value in JSON (include the double quotes): "{\"key\":\"value\"}"
//     - a list value in JSON (include the double quotes): "[1,true,nil,\"string\"]"
//
// CosmosDB automatically creates a few extra fields for the insert document.
// See https://docs.microsoft.com/en-us/azure/cosmos-db/account-databases-containers-items#properties-of-an-item
type StmtInsert struct {
	*Stmt
	dbName    string
	collName  string
	isUpsert  bool
	fieldsStr string
	valuesStr string
	fields    []string
	values    []interface{}
}

func (s *StmtInsert) parse() error {
	s.fields = regexp.MustCompile(`[,\s]+`).Split(s.fieldsStr, -1)
	s.values = make([]interface{}, 0)
	s.numInput = 1
	for temp := strings.TrimSpace(s.valuesStr); temp != ""; temp = strings.TrimSpace(temp) {
		if loc := reValPlaceholder.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]+1:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			s.numInput++
			index, _ := strconv.Atoi(token)
			s.values = append(s.values, placeholder{index})
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValNull.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			s.values = append(s.values, nil)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValNumber.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			var data interface{}
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(nul) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValBoolean.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			var data bool
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(bool) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValString.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token, err := strconv.Unquote(strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' }))
			if err != nil {
				return errors.New("(unquote) cannot parse query, invalid token at: " + token)
			}
			var data interface{}
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(unmarshal) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		return errors.New("cannot parse query, invalid token at: " + temp)
	}

	return nil
}

func (s *StmtInsert) validate() error {
	if len(s.fields) != len(s.values) {
		return fmt.Errorf("number of field (%d) does not match number of input value (%d)", len(s.fields), len(s.values))
	}
	return nil
}

// Exec implements driver.Stmt.Exec.
// Upon successful call, this function returns (*ResultInsert, nil).
//
// Note: this function expects the last argument is partition key value.
func (s *StmtInsert) Exec(args []driver.Value) (driver.Result, error) {
	spec := DocumentSpec{
		DbName:             s.dbName,
		CollName:           s.collName,
		IsUpsert:           s.isUpsert,
		PartitionKeyValues: []interface{}{args[s.numInput-1]}, // expect the last argument is partition key value
		DocumentData:       make(map[string]interface{}),
	}
	for i := 0; i < len(s.fields); i++ {
		switch s.values[i].(type) {
		case placeholder:
			ph := s.values[i].(placeholder)
			if ph.index <= 0 || ph.index >= len(args) {
				return nil, fmt.Errorf("invalid value index %d", ph.index)
			}
			spec.DocumentData[s.fields[i]] = args[ph.index-1]
		default:
			spec.DocumentData[s.fields[i]] = s.values[i]
		}
	}
	restResult := s.conn.restClient.CreateDocument(spec)
	result := &ResultInsert{
		Successful: restResult.Error() == nil,
		// StatusCode:   restResult.StatusCode,
		// SessionToken: restResult.SessionToken,
		// RUCharge:     restResult.RequestCharge,
	}
	if restResult.DocInfo != nil {
		result.InsertId, _ = restResult.DocInfo["_rid"].(string)
	}
	err := restResult.Error()
	switch restResult.StatusCode {
	case 403:
		err = ErrForbidden
	case 404:
		err = ErrNotFound
	case 409:
		err = ErrConflict
	}
	return result, err
}

// Query implements driver.Stmt.Query.
// This function is not implemented, use Exec instead.
func (s *StmtInsert) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("this operation is not supported, please use exec")
}

// ResultInsert captures the result from INSERT operation.
type ResultInsert struct {
	// Successful flags if the operation was successful or not.
	Successful bool
	// // StatusCode is the HTTP status code returned from CosmosDB.
	// StatusCode int
	// InsertId holds the "_rid" if the operation was successful.
	InsertId string
	// // RUCharge holds the number of request units consumed by the operation.
	// RUCharge float64
	// // SessionToken is the string token used with session level consistency.
	// // Clients must save this value and set it for subsequent read requests for session consistency.
	// SessionToken string
}

// LastInsertId implements driver.Result.LastInsertId.
func (r *ResultInsert) LastInsertId() (int64, error) {
	return 0, fmt.Errorf("this operation is not supported. {LastInsertId:%s}", r.InsertId)
}

// RowsAffected implements driver.Result.RowsAffected.
func (r *ResultInsert) RowsAffected() (int64, error) {
	if r.Successful {
		return 1, nil
	}
	return 0, nil
}

/*----------------------------------------------------------------------*/

// StmtDelete implements "DELETE" operation.
//
// Syntax: DELETE FROM <db-name>.<collection-name> WHERE id=<id-value>
//
// - currently DELETE only removes one document specified by id.
// - <id-value> is treated as a string. Either `WHERE id=abc` or `WHERE id="abc"` is accepted.
type StmtDelete struct {
	*Stmt
	dbName   string
	collName string
	idStr    string
	id       interface{}
}

func (s *StmtDelete) parse() error {
	s.numInput = 1
	hasPrefix := strings.HasPrefix(s.idStr, `"`)
	hasSuffix := strings.HasSuffix(s.idStr, `"`)
	if hasPrefix != hasSuffix {
		return fmt.Errorf("invalid id literate: %s", s.idStr)
	}
	if hasPrefix && hasSuffix {
		s.idStr = strings.TrimSpace(s.idStr[1 : len(s.idStr)-1])
	} else if loc := reValPlaceholder.FindStringIndex(s.idStr); loc != nil {
		if loc[0] == 0 && loc[1] == len(s.idStr) {
			index, err := strconv.Atoi(s.idStr[loc[0]+1:])
			if err != nil || index < 1 {
				return fmt.Errorf("invalid id placeholder literate: %s", s.idStr)
			}
			s.id = placeholder{index}
			s.numInput++
		} else {
			return fmt.Errorf("invalid id literate: %s", s.idStr)
		}
	}
	return nil
}

func (s *StmtDelete) validate() error {
	if s.idStr == "" {
		return errors.New("id value is missing")
	}
	return nil
}

// Exec implements driver.Stmt.Exec.
// This function always return nil driver.Result.
//
// Note: this function expects the last argument is partition key value.
func (s *StmtDelete) Exec(args []driver.Value) (driver.Result, error) {
	id := s.idStr
	if s.id != nil {
		ph := s.id.(placeholder)
		if ph.index <= 0 || ph.index >= len(args) {
			return nil, fmt.Errorf("invalid value index %d", ph.index)
		}
		id = fmt.Sprintf("%s", args[ph.index-1])
	}
	restClient := s.conn.restClient.DeleteDocument(DocReq{DbName: s.dbName, CollName: s.collName, DocId: id,
		PartitionKeyValues: []interface{}{args[s.numInput-1]}, // expect the last argument is partition key value
	})
	err := restClient.Error()
	result := &ResultDelete{Successful: err == nil, StatusCode: restClient.StatusCode}
	switch restClient.StatusCode {
	case 403:
		err = ErrForbidden
	case 404:
		// consider "document not found" as successful operation
		// but database/collection not found is not!
		if strings.Index(err.Error(), "ResourceType: Document") >= 0 {
			err = nil
		} else {
			err = ErrNotFound
		}
	}
	return result, err
}

// Query implements driver.Stmt.Query.
// This function is not implemented, use Exec instead.
func (s *StmtDelete) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("this operation is not supported, please use exec")
}

// ResultDelete captures the result from DELETE operation.
type ResultDelete struct {
	// Successful flags if the operation was successful or not.
	Successful bool
	// StatusCode is the HTTP status code returned from CosmosDB.
	StatusCode int
	// // RUCharge holds the number of request units consumed by the operation.
	// RUCharge float64
	// // SessionToken is the string token used with session level consistency.
	// // Clients must save this value and set it for subsequent read requests for session consistency.
	// SessionToken string
}

// LastInsertId implements driver.Result.LastInsertId.
func (r *ResultDelete) LastInsertId() (int64, error) {
	return 0, errors.New("this operation is not supported")
}

// RowsAffected implements driver.Result.RowsAffected.
func (r *ResultDelete) RowsAffected() (int64, error) {
	if r.Successful && r.StatusCode < 400 {
		return 1, nil
	}
	return 0, nil
}

/*----------------------------------------------------------------------*/

// StmtSelect implements "SELECT" operation.
//
// The "SELECT" query follows CosmosDB's SQL grammar (https://docs.microsoft.com/en-us/azure/cosmos-db/sql-query-getting-started)
// with a few extensions:
// - Syntax: SELECT [CROSS PARTITION] ... FROM <collection/table-name> ... WITH database|db=<db-name> [WITH collection|table=<collection/table-name>] [WITH cross_partition=true]
// - (extension) If the collection is partitioned, specify CROSS PARTITION to allow execution across multiple partitions.
//   This clause is not required if query is to be executed on a single partition.
//   Cross-partition execution can also be enabled using WITH cross_partition=true.
// - (extension) Use WITH database=<db-name> (or WITH db=<db-name>) to specify the database on which the query is to be executed.
// - (extension) Use WITH collection=<coll-name> (or WITH table=<coll-name>) to specify the collection/table on which the query is to be executed.
//   If not specified, collection/table name is extracted from the "FROM <collection/table-name>" clause.
// - (extension) Use placeholder syntax @i, $i or :i (where i denotes the i-th parameter, the first parameter is 1)
type StmtSelect struct {
	*Stmt
	isCrossPartition bool
	dbName           string
	collName         string
	selectQuery      string
	placeholders     map[int]string
}

func (s *StmtSelect) parse(withOptsStr string) error {
	if err := s.Stmt.parseWithOpts(withOptsStr); err != nil {
		return err
	}
	if v, ok := s.withOpts["DATABASE"]; ok {
		s.dbName = strings.TrimSpace(v)
	} else if v, ok := s.withOpts["DB"]; ok {
		s.dbName = strings.TrimSpace(v)
	}
	if v, ok := s.withOpts["COLLECTION"]; ok {
		s.collName = strings.TrimSpace(v)
	} else if v, ok := s.withOpts["TABLE"]; ok {
		s.collName = strings.TrimSpace(v)
	}
	if v, ok := s.withOpts["CROSS_PARTITION"]; ok && !s.isCrossPartition {
		vbool, err := strconv.ParseBool(v)
		if err != nil || !vbool {
			return errors.New("cannot parse query (the only accepted value for cross_partition is true), invalid token at: " + v)
		}
		s.isCrossPartition = true
	}

	matches := reValPlaceholder.FindAllStringSubmatch(s.selectQuery, -1)
	s.numInput = len(matches)
	s.placeholders = make(map[int]string)
	for _, match := range matches {
		if v, err := strconv.Atoi(match[1]); err == nil {
			key := "@_" + match[1]
			s.placeholders[v] = key
			if strings.HasSuffix(match[0], " ") {
				key += " "
			}
			s.selectQuery = strings.ReplaceAll(s.selectQuery, match[0], key)
		} else {
			return errors.New("cannot parse query, invalid token at: " + match[0])
		}
	}

	return nil
}

func (s *StmtSelect) validate() error {
	if s.dbName == "" || s.collName == "" {
		return errors.New("database or collection is not specified")
	}
	return nil
}

// Query implements driver.Stmt.Query.
// Upon successful call, this function returns (*ResultSelect, nil).
func (s *StmtSelect) Query(args []driver.Value) (driver.Rows, error) {
	params := make([]interface{}, 0)
	for i, arg := range args {
		if v, ok := s.placeholders[i+1]; !ok {
			return nil, fmt.Errorf("there is placeholder number %d", i+1)
		} else {
			params = append(params, map[string]interface{}{"name": fmt.Sprintf("%s", v), "value": arg})
		}
	}
	query := QueryReq{
		DbName:                s.dbName,
		CollName:              s.collName,
		Query:                 s.selectQuery,
		Params:                params,
		CrossPartitionEnabled: s.isCrossPartition,
	}
	documents := make([]DocInfo, 0)
	var restResult *RespQueryDocs
	for restResult = s.conn.restClient.QueryDocuments(query); restResult.Error() == nil; {
		documents = append(documents, restResult.Documents...)
		if restResult.ContinuationToken == "" {
			break
		}
		query.ContinuationToken = restResult.ContinuationToken
		restResult = s.conn.restClient.QueryDocuments(query)
	}
	err := restResult.Error()
	var rows driver.Rows
	if err == nil {
		rows = &ResultSelect{
			count:       int(restResult.Count),
			documents:   documents,
			cursorCount: 0,
			columnList:  make([]string, 0),
		}
		if len(documents) > 0 {
			doc := documents[0]
			columnList := make([]string, len(doc))
			i := 0
			for colName := range doc {
				columnList[i] = colName
				i++
			}
			sort.Strings(columnList)
			rows.(*ResultSelect).columnList = columnList
		}
	}
	switch restResult.StatusCode {
	case 403:
		err = ErrForbidden
	case 404:
		err = ErrNotFound
	case 409:
		err = ErrConflict
	}
	return rows, err
}

// Exec implements driver.Stmt.Exec.
// This function is not implemented, use Query instead.
func (s *StmtSelect) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("this operation is not supported, please use query")
}

// ResultSelect captures the result from SELECT operation.
type ResultSelect struct {
	count       int
	documents   []DocInfo
	cursorCount int
	columnList  []string
}

// Columns implements driver.Rows.Columns.
func (r *ResultSelect) Columns() []string {
	return r.columnList
}

// Close implements driver.Rows.Close.
func (r *ResultSelect) Close() error {
	return nil
}

// Next implements driver.Rows.Next.
func (r *ResultSelect) Next(dest []driver.Value) error {
	if r.cursorCount >= r.count {
		return io.EOF
	}
	rowData := r.documents[r.cursorCount]
	r.cursorCount++
	for i, colName := range r.columnList {
		dest[i] = rowData[colName]
	}
	return nil
}

/*----------------------------------------------------------------------*/

// StmtUpdate implements "UPDATE" operation.
//
// Syntax: UPDATE <db-name>.<collection-name> SET <field-name>=<value>[,<field-name>=<value>]*, WHERE id=<id-value>
//
// - currently UPDATE only updates one document specified by id.
// - <id-value> is treated as a string. Either `WHERE id=abc` or `WHERE id="abc"` is accepted.
// - <value> is either:
//   - a placeholder (e.g. :1, @2 or $3)
//   - a null
//   - a number
//   - a boolean (true/false)
//   - a string (inside double quotes) that must be a valid JSON, e.g.
//     - a string value in JSON (include the double quotes): "\"a string\""
//     - a number value in JSON (include the double quotes): "123"
//     - a boolean value in JSON (include the double quotes): "true"
//     - a null value in JSON (include the double quotes): "null"
//     - a map value in JSON (include the double quotes): "{\"key\":\"value\"}"
//     - a list value in JSON (include the double quotes): "[1,true,nil,\"string\"]"
type StmtUpdate struct {
	*Stmt
	dbName    string
	collName  string
	updateStr string
	idStr     string
	id        interface{}
	fields    []string
	values    []interface{}
}

func (s *StmtUpdate) _parseId() error {
	hasPrefix := strings.HasPrefix(s.idStr, `"`)
	hasSuffix := strings.HasSuffix(s.idStr, `"`)
	if hasPrefix != hasSuffix {
		return fmt.Errorf("invalid id literate: %s", s.idStr)
	}
	if hasPrefix && hasSuffix {
		s.idStr = strings.TrimSpace(s.idStr[1 : len(s.idStr)-1])
	} else if loc := reValPlaceholder.FindStringIndex(s.idStr); loc != nil {
		if loc[0] == 0 && loc[1] == len(s.idStr) {
			index, err := strconv.Atoi(s.idStr[loc[0]+1:])
			if err != nil || index < 1 {
				return fmt.Errorf("invalid id placeholder literate: %s", s.idStr)
			}
			s.id = placeholder{index}
			s.numInput++
		} else {
			return fmt.Errorf("invalid id literate: %s", s.idStr)
		}
	}
	return nil
}

var (
	reFieldPart = regexp.MustCompile(`\s*([\w\-]+)\s*=`)
)

func _isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0, '=':
		return true
	}
	return false
}

func (s *StmtUpdate) _parseUpdateClause() error {
	s.fields = make([]string, 0)
	s.values = make([]interface{}, 0)
	for temp := strings.TrimSpace(s.updateStr); temp != ""; temp = strings.TrimSpace(temp) {
		// firstly, extract the field name
		if loc := reFieldPart.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			field := strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == '=' })
			s.fields = append(s.fields, field)
			temp = strings.TrimSpace(temp[loc[1]:])
		} else {
			return errors.New("(field) cannot parse query, invalid token at: " + temp[loc[0]:loc[1]])
		}

		// secondly, parse the value part
		if loc := reValPlaceholder.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]+1:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			s.numInput++
			index, _ := strconv.Atoi(token)
			s.values = append(s.values, placeholder{index})
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValNull.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			s.values = append(s.values, nil)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValNumber.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			var data interface{}
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(nul) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValBoolean.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token := strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' })
			var data bool
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(bool) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		if loc := reValString.FindStringIndex(temp); loc != nil && loc[0] == 0 {
			token, err := strconv.Unquote(strings.TrimFunc(temp[loc[0]:loc[1]], func(r rune) bool { return _isSpace(r) || r == ',' }))
			if err != nil {
				fmt.Println(err)
				return errors.New("(unquote) cannot parse query, invalid token at: " + token)
			}
			var data interface{}
			if err := json.Unmarshal([]byte(token), &data); err != nil {
				return errors.New("(unmarshal) cannot parse query, invalid token at: " + token)
			}
			s.values = append(s.values, data)
			temp = temp[loc[1]:]
			continue
		}
		return errors.New("cannot parse query, invalid token at: " + temp)
	}
	return nil
}

func (s *StmtUpdate) parse() error {
	s.numInput = 1

	if err := s._parseId(); err != nil {
		return err
	}

	if err := s._parseUpdateClause(); err != nil {
		return err
	}

	return nil
}

func (s *StmtUpdate) validate() error {
	if s.idStr == "" {
		return errors.New("id value is missing")
	}
	if len(s.fields) == 0 {
		return errors.New("invalid query: SET clause is empty")
	}
	if len(s.fields) != len(s.values) {
		return fmt.Errorf("number of field (%d) does not match number of input value (%d)", len(s.fields), len(s.values))
	}
	return nil
}

// Exec implements driver.Stmt.Exec.
// Upon successful call, this function returns (*ResultUpdate, nil).
//
// Note: this function expects the last argument is partition key value.
func (s *StmtUpdate) Exec(args []driver.Value) (driver.Result, error) {
	// firstly, fetch the document
	id := s.idStr
	if s.id != nil {
		ph := s.id.(placeholder)
		if ph.index <= 0 || ph.index >= len(args) {
			return nil, fmt.Errorf("invalid value index %d", ph.index)
		}
		id = fmt.Sprintf("%s", args[ph.index-1])
	}
	docReq := DocReq{DbName: s.dbName, CollName: s.collName, DocId: id, PartitionKeyValues: []interface{}{args[len(args)-1]}}
	getDocResult := s.conn.restClient.GetDocument(docReq)
	if err := getDocResult.Error(); err != nil {
		if getDocResult.StatusCode == 404 {
			// consider "document not found" as successful operation
			// but database/collection not found is not!
			if strings.Index(err.Error(), "ResourceType: Document") >= 0 {
				return &ResultUpdate{Successful: false}, nil
			}
			return nil, ErrNotFound
		}
		return nil, getDocResult.Error()
	}
	etag := getDocResult.DocInfo.Etag()
	spec := DocumentSpec{DbName: s.dbName, CollName: s.collName, PartitionKeyValues: []interface{}{args[len(args)-1]}, DocumentData: getDocResult.DocInfo.RemoveSystemAttrs()}
	for i := 0; i < len(s.fields); i++ {
		switch s.values[i].(type) {
		case placeholder:
			ph := s.values[i].(placeholder)
			if ph.index <= 0 || ph.index >= len(args) {
				return nil, fmt.Errorf("invalid value index %d", ph.index)
			}
			spec.DocumentData[s.fields[i]] = args[ph.index-1]
		default:
			spec.DocumentData[s.fields[i]] = s.values[i]
		}
	}
	replaceDocResult := s.conn.restClient.ReplaceDocument(etag, spec)
	result := &ResultUpdate{Successful: replaceDocResult.Error() == nil}
	err := replaceDocResult.Error()
	switch replaceDocResult.StatusCode {
	case 403:
		err = ErrForbidden
	case 404:
		// consider "document not found" as successful operation
		// but database/collection not found is not!
		if strings.Index(err.Error(), "ResourceType: Document") >= 0 {
			err = nil
		} else {
			err = ErrNotFound
		}
	case 409:
		err = ErrConflict
	case 412:
		err = nil
	}
	return result, err
}

// Query implements driver.Stmt.Query.
// This function is not implemented, use Exec instead.
func (s *StmtUpdate) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("this operation is not supported, please use exec")
}

// ResultUpdate captures the result from UPDATE operation.
type ResultUpdate struct {
	// Successful flags if the operation was successful or not.
	Successful bool
	// // StatusCode is the HTTP status code returned from CosmosDB.
	// StatusCode int
	// // RUCharge holds the number of request units consumed by the operation.
	// RUCharge float64
	// // SessionToken is the string token used with session level consistency.
	// // Clients must save this value and set it for subsequent read requests for session consistency.
	// SessionToken string
}

// LastInsertId implements driver.Result.LastInsertId.
func (r *ResultUpdate) LastInsertId() (int64, error) {
	return 0, errors.New("this operation is not supported")
}

// RowsAffected implements driver.Result.RowsAffected.
func (r *ResultUpdate) RowsAffected() (int64, error) {
	if r.Successful {
		return 1, nil
	}
	return 0, nil
}