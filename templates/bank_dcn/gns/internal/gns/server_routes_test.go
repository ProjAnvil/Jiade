package gns

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"bank-dcn/lib/sqltest"
)

func dupEntry() error {
	return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
}

func doReq(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	var rb io.Reader
	if body != "" {
		rb = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, rb))
	return rec
}

// fakeRedis is a minimal in-process RESP2 server so cache code paths can run
// without a live Redis: it answers GET/SET and rejects every other command.
// The client is configured with Protocol 2, which skips the HELLO handshake.
type fakeRedis struct {
	mu   sync.Mutex
	sets map[string]string
}

func startFakeRedis(t *testing.T) (*fakeRedis, *redis.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRedis{sets: map[string]string{}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go fr.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	client := redis.NewClient(&redis.Options{
		Addr: ln.Addr().String(), Protocol: 2, MaxRetries: -1,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	t.Cleanup(func() { client.Close() })
	return fr, client
}

func (fr *fakeRedis) set(key string) (string, bool) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	v, ok := fr.sets[key]
	return v, ok
}

func (fr *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToUpper(args[0]) {
		case "GET":
			fr.mu.Lock()
			v, ok := fr.sets[args[1]]
			fr.mu.Unlock()
			if !ok {
				_, _ = c.Write([]byte("$-1\r\n"))
			} else {
				fmt.Fprintf(c, "$%d\r\n%s\r\n", len(v), v)
			}
		case "SET":
			if len(args) >= 3 {
				fr.mu.Lock()
				fr.sets[args[1]] = args[2]
				fr.mu.Unlock()
			}
			_, _ = c.Write([]byte("+OK\r\n"))
		default:
			_, _ = c.Write([]byte("-ERR unsupported command\r\n"))
		}
	}
}

// readCommand parses one RESP array-of-bulk-strings request.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return readCommand(r)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(line, "*"))
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("fake redis: bad array header %q", line)
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		size, err := strconv.Atoi(strings.TrimPrefix(hdr, "$"))
		if err != nil || size < 0 {
			return nil, fmt.Errorf("fake redis: bad bulk header %q", hdr)
		}
		buf := make([]byte, size+2) // value plus trailing CRLF
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func TestCacheKey(t *testing.T) {
	if got := cacheKey(1001); got != "route:1001" {
		t.Fatalf("cacheKey(1001) = %q, want route:1001", got)
	}
}

// Handler routing: every registered route reaches its handler; unmatched
// methods and paths fall through to the mux defaults (405/404).
func TestHandlerRouting(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.account_id = ?",
			Columns: []string{"dcn", "endpoint"},
			Rows:    [][]any{{"dcn01", "http://dcn01-app:8080"}}},
		sqltest.Rule{Contains: "ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"},
			Rows:    [][]any{{int64(1001), "dcn01", "http://dcn01-app:8080"}}},
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows: [][]any{{"dcn01", int64(1000), int64(1999),
				"http://dcn01-app:8080", "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}},
		sqltest.Rule{Contains: "INSERT INTO route_segment"},
	)
	h := NewServer(db, deadRedis()).Handler()

	cases := []struct {
		name, method, target, body string
		wantCode                   int
		wantSub                    string
	}{
		{"healthz", "GET", "/healthz", "", 200, `"status":"ok"`},
		{"locate found", "GET", "/locate?accountId=1001", "", 200, "dcn01"},
		{"locate bad param", "GET", "/locate?accountId=abc", "", 400, "accountId"},
		{"open account idempotent hit", "POST", "/accounts",
			`{"name":"a","initBalance":"1.00","requestId":"r-1"}`, 200, "1001"},
		{"list routes", "GET", "/routes", "", 200, "dcn01"},
		{"add route", "POST", "/routes",
			`{"dcn":"dcn09","segStart":9000,"segEnd":9499,"endpoint":"http://dcn09-app:8080"}`,
			201, "ACTIVE"},
		{"method not allowed", "POST", "/locate?accountId=1001", "", 405, ""},
		{"unknown path", "GET", "/unknown", "", 404, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doReq(h, c.method, c.target, c.body)
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantSub != "" && !strings.Contains(rec.Body.String(), c.wantSub) {
				t.Fatalf("body %s should contain %q", rec.Body, c.wantSub)
			}
		})
	}
}

// Locate on cache miss reads MySQL and backfills the cache; the next call is
// served from the cache without touching MySQL again.
func TestLocateMissThenHit(t *testing.T) {
	fr, client := startFakeRedis(t)
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.account_id = ?",
			Columns: []string{"dcn", "endpoint"},
			Rows:    [][]any{{"dcn01", "http://dcn01-app:8080"}}},
	)
	s := NewServer(db, client)

	res, err := s.Locate(context.Background(), 1001)
	if err != nil || res.AccountID != 1001 || res.DCN != "dcn01" ||
		res.Endpoint != "http://dcn01-app:8080" {
		t.Fatalf("Locate = %+v, %v", res, err)
	}
	if v, ok := fr.set("route:1001"); !ok || v != "dcn01|http://dcn01-app:8080" {
		t.Fatalf("cache backfill = %q, %v", v, ok)
	}
	res, err = s.Locate(context.Background(), 1001)
	if err != nil || res.DCN != "dcn01" || res.Endpoint != "http://dcn01-app:8080" {
		t.Fatalf("cached Locate = %+v, %v", res, err)
	}
	if rec.QueryCount("ar.account_id") != 1 {
		t.Fatalf("MySQL should be queried exactly once, got %d",
			rec.QueryCount("ar.account_id"))
	}
}

// Locate: no routing row → ErrNotFound (404 over HTTP); a generic DB failure
// is surfaced as a 500.
func TestLocateNotFoundAndDBError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.account_id = ?",
			Columns: []string{"dcn", "endpoint"}}, // empty set
	)
	s := NewServer(db, deadRedis())
	if _, err := s.Locate(context.Background(), 42); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	rec := httptest.NewRecorder()
	s.handleLocate(rec, httptest.NewRequest("GET", "/locate?accountId=42", nil))
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body)
	}

	dbErr, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.account_id = ?", Err: fmt.Errorf("db down")},
	)
	sErr := NewServer(dbErr, deadRedis())
	recErr := httptest.NewRecorder()
	sErr.handleLocate(recErr, httptest.NewRequest("GET", "/locate?accountId=42", nil))
	if recErr.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", recErr.Code, recErr.Body)
	}
}

// handleListRoutes: happy path returns the segment list; query and scan
// failures return 500.
func TestHandleListRoutes(t *testing.T) {
	segRow := sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
		Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
		Rows: [][]any{{"dcn01", int64(1000), int64(1999),
			"http://dcn01-app:8080", "ACTIVE"}}}

	db, _ := sqltest.NewDB(t, segRow)
	s := NewServer(db, deadRedis())
	rec := httptest.NewRecorder()
	s.handleListRoutes(rec, httptest.NewRequest("GET", "/routes", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body)
	}
	var segs []Segment
	if err := json.Unmarshal(rec.Body.Bytes(), &segs); err != nil || len(segs) != 1 ||
		segs[0].DCN != "dcn01" || segs[0].Status != "ACTIVE" {
		t.Fatalf("routes = %s", rec.Body)
	}

	dbQ, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM route_segment", Err: fmt.Errorf("db down")})
	recQ := httptest.NewRecorder()
	NewServer(dbQ, deadRedis()).handleListRoutes(recQ, httptest.NewRequest("GET", "/routes", nil))
	if recQ.Code != 500 {
		t.Fatalf("query error: status = %d, want 500", recQ.Code)
	}

	// 4 columns cannot fill the 5 scan targets → scan error → 500
	dbS, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint"},
			Rows: [][]any{{"dcn01", int64(1000), int64(1999),
				"http://dcn01-app:8080"}}})
	recS := httptest.NewRecorder()
	NewServer(dbS, deadRedis()).handleListRoutes(recS, httptest.NewRequest("GET", "/routes", nil))
	if recS.Code != 500 {
		t.Fatalf("scan error: status = %d, want 500", recS.Code)
	}
}

// handleAddRoute: created, duplicate-dcn idempotency, insert failure and
// validation errors.
func TestHandleAddRoute(t *testing.T) {
	body := `{"dcn":"dcn09","segStart":9000,"segEnd":9499,"endpoint":"http://dcn09-app:8080"}`

	t.Run("created", func(t *testing.T) {
		db, rec := sqltest.NewDB(t, sqltest.Rule{Contains: "INSERT INTO route_segment"})
		recw := httptest.NewRecorder()
		NewServer(db, deadRedis()).handleAddRoute(recw,
			httptest.NewRequest("POST", "/routes", strings.NewReader(body)))
		if recw.Code != 201 {
			t.Fatalf("status = %d (body: %s)", recw.Code, recw.Body)
		}
		var seg Segment
		if err := json.Unmarshal(recw.Body.Bytes(), &seg); err != nil ||
			seg.Status != "ACTIVE" || seg.DCN != "dcn09" {
			t.Fatalf("resp = %s", recw.Body)
		}
		ins := rec.LastExec("INSERT INTO route_segment")
		if len(ins.Args) != 4 || ins.Args[0] != "dcn09" || ins.Args[1] != int64(9000) ||
			ins.Args[2] != int64(9499) || ins.Args[3] != "http://dcn09-app:8080" {
			t.Fatalf("insert args = %v", ins.Args)
		}
	})

	t.Run("duplicate dcn is idempotent", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO route_segment", Err: dupEntry()})
		recw := httptest.NewRecorder()
		NewServer(db, deadRedis()).handleAddRoute(recw,
			httptest.NewRequest("POST", "/routes", strings.NewReader(body)))
		if recw.Code != 200 || !strings.Contains(recw.Body.String(), `"exists"`) {
			t.Fatalf("duplicate: %d %s", recw.Code, recw.Body)
		}
	})

	t.Run("insert failure", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO route_segment", Err: fmt.Errorf("db down")})
		recw := httptest.NewRecorder()
		NewServer(db, deadRedis()).handleAddRoute(recw,
			httptest.NewRequest("POST", "/routes", strings.NewReader(body)))
		if recw.Code != 500 {
			t.Fatalf("status = %d, want 500", recw.Code)
		}
	})

	for _, c := range []struct{ name, body string }{
		{"malformed json", `{"dcn":`},
		{"missing dcn", `{"segStart":9000,"segEnd":9499,"endpoint":"http://e"}`},
		{"missing endpoint", `{"dcn":"dcn09","segStart":9000,"segEnd":9499}`},
		{"segStart not positive", `{"dcn":"dcn09","segStart":0,"segEnd":9499,"endpoint":"http://e"}`},
		{"segEnd below segStart", `{"dcn":"dcn09","segStart":9000,"segEnd":8999,"endpoint":"http://e"}`},
	} {
		t.Run("validation: "+c.name, func(t *testing.T) {
			db, rec := sqltest.NewDB(t) // no rules: validation must reject before any SQL
			recw := httptest.NewRecorder()
			NewServer(db, deadRedis()).handleAddRoute(recw,
				httptest.NewRequest("POST", "/routes", strings.NewReader(c.body)))
			if recw.Code != 400 {
				t.Fatalf("status = %d, want 400 (body: %s)", recw.Code, recw.Body)
			}
			if rec.ExecCount("") != 0 {
				t.Fatal("invalid payload must not reach the database")
			}
		})
	}
}

// handleOpenAccount validation: malformed JSON or missing fields → 400 before
// any statement runs.
func TestOpenAccountValidation(t *testing.T) {
	db, rec := sqltest.NewDB(t)
	s := NewServer(db, deadRedis())
	for _, body := range []string{
		`{"name":`,                 // malformed JSON
		`{"initBalance":"100.00"}`, // missing name
		`{"name":"alice"}`,         // missing initBalance
	} {
		recw := httptest.NewRecorder()
		s.handleOpenAccount(recw, openReq(body))
		if recw.Code != 400 {
			t.Fatalf("body %s: status = %d, want 400", body, recw.Code)
		}
	}
	if rec.ExecCount("") != 0 || rec.QueryCount("") != 0 {
		t.Fatal("invalid payload must not reach the database")
	}
}

// Happy path: the least-loaded ACTIVE segment is picked, the account is created
// in the DCN, the route cache is warmed and 201 is returned.
func TestOpenAccountSuccess(t *testing.T) {
	var dcnBody map[string]any
	dcn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts" {
			t.Errorf("dcn path = %s, want /accounts", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&dcnBody); err != nil {
			t.Errorf("dcn body: %v", err)
		}
		w.WriteHeader(200)
	}))
	defer dcn.Close()

	fr, client := startFakeRedis(t)
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"}}, // no rows: fresh request
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows: [][]any{
				{"dcn01", int64(1000), int64(1999), "http://dcn01-app:8080", "ACTIVE"},
				{"dcn02", int64(2000), int64(2999), dcn.URL, "ACTIVE"},
			}},
		sqltest.Rule{Contains: "GROUP BY dcn",
			Columns: []string{"dcn", "COUNT(*)"},
			Rows:    [][]any{{"dcn01", int64(5)}}}, // dcn02 has fewer accounts → picked
		sqltest.Rule{Contains: "MAX(account_id)",
			Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}},
		sqltest.Rule{Contains: "INSERT INTO account_route"},
	)
	s := NewServer(db, client)
	recw := httptest.NewRecorder()
	s.handleOpenAccount(recw, openReq(`{"name":"carol","initBalance":"250.00","requestId":"r-9"}`))
	if recw.Code != 201 {
		t.Fatalf("status = %d (body: %s)", recw.Code, recw.Body)
	}
	var v LocateResult
	if err := json.Unmarshal(recw.Body.Bytes(), &v); err != nil ||
		v.AccountID != 2001 || v.DCN != "dcn02" || v.Endpoint != dcn.URL {
		t.Fatalf("resp = %s", recw.Body)
	}
	ins := rec.LastExec("INSERT INTO account_route")
	if len(ins.Args) != 3 || ins.Args[0] != int64(2001) ||
		ins.Args[1] != "dcn02" || ins.Args[2] != "r-9" {
		t.Fatalf("insert args = %v", ins.Args)
	}
	if dcnBody["name"] != "carol" || dcnBody["initBalance"] != "250.00" ||
		dcnBody["accountId"] != float64(2001) {
		t.Fatalf("dcn payload = %v", dcnBody)
	}
	if v, ok := fr.set("route:2001"); !ok || v != "dcn02|"+dcn.URL {
		t.Fatalf("cache warm-up = %q, %v", v, ok)
	}
}

// Without a requestId the 2-argument insert form is used and no idempotency
// lookup happens.
func TestOpenAccountSuccessNoRequestID(t *testing.T) {
	dcn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer dcn.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows:    [][]any{{"dcn01", int64(1000), int64(1999), dcn.URL, "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}},
		sqltest.Rule{Contains: "MAX(account_id)",
			Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}},
		sqltest.Rule{Contains: "INSERT INTO account_route"},
	)
	s := NewServer(db, deadRedis())
	recw := httptest.NewRecorder()
	s.handleOpenAccount(recw, openReq(`{"name":"dave","initBalance":"10.00"}`))
	if recw.Code != 201 {
		t.Fatalf("status = %d (body: %s)", recw.Code, recw.Body)
	}
	if rec.QueryCount("ar.request_id") != 0 {
		t.Fatal("no idempotency lookup without requestId")
	}
	ins := rec.LastExec("INSERT INTO account_route")
	if len(ins.Args) != 2 || ins.Args[0] != int64(1001) || ins.Args[1] != "dcn01" {
		t.Fatalf("insert args = %v, want [1001 dcn01]", ins.Args)
	}
}

// Only DRAINING segments exist → 503 without any allocation attempt.
func TestOpenAccountNoActiveSegment(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows: [][]any{{"dcn03", int64(3000), int64(3999),
				"http://dcn03-app:8080", "DRAINING"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}},
	)
	s := NewServer(db, deadRedis())
	recw := httptest.NewRecorder()
	s.handleOpenAccount(recw, openReq(`{"name":"erin","initBalance":"1.00"}`))
	if recw.Code != 503 || !strings.Contains(recw.Body.String(), "no ACTIVE segment") {
		t.Fatalf("resp = %d %s", recw.Code, recw.Body)
	}
	if rec.ExecCount("INSERT") != 0 {
		t.Fatal("no allocation must happen without an ACTIVE segment")
	}
}

// segment listing and account-count failures surface as 500.
func TestOpenAccountDBFailures(t *testing.T) {
	t.Run("segment listing fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FROM route_segment", Err: fmt.Errorf("db down")})
		recw := httptest.NewRecorder()
		NewServer(db, deadRedis()).handleOpenAccount(recw,
			openReq(`{"name":"a","initBalance":"1.00"}`))
		if recw.Code != 500 {
			t.Fatalf("status = %d, want 500", recw.Code)
		}
	})
	t.Run("account counts fail", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
				Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
				Rows: [][]any{{"dcn01", int64(1000), int64(1999),
					"http://dcn01-app:8080", "ACTIVE"}}},
			sqltest.Rule{Contains: "GROUP BY dcn", Err: fmt.Errorf("db down")},
		)
		recw := httptest.NewRecorder()
		NewServer(db, deadRedis()).handleOpenAccount(recw,
			openReq(`{"name":"a","initBalance":"1.00"}`))
		if recw.Code != 500 {
			t.Fatalf("status = %d, want 500", recw.Code)
		}
	})
}

// allocate failing inside handleOpenAccount (segment already full) → 503.
func TestOpenAccountAllocationFails(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"}},
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows: [][]any{{"dcn01", int64(1000), int64(1999),
				"http://dcn01-app:8080", "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}},
		maxRule([][]any{{int64(1999)}}), // segment is full
	)
	s := NewServer(db, deadRedis())
	recw := httptest.NewRecorder()
	s.handleOpenAccount(recw, openReq(`{"name":"gina","initBalance":"1.00"}`))
	if recw.Code != 503 || !strings.Contains(recw.Body.String(), "full") {
		t.Fatalf("resp = %d %s", recw.Code, recw.Body)
	}
}

var seg1000 = Segment{DCN: "dcn01", SegStart: 1000, SegEnd: 1999,
	Endpoint: "http://dcn01-app:8080", Status: "ACTIVE"}

func maxRule(rows [][]any) sqltest.Rule {
	return sqltest.Rule{Contains: "MAX(account_id)",
		Columns: []string{"MAX(account_id)"}, Rows: rows}
}

// allocate: a full segment, MAX query failure and non-duplicate insert errors
// abort immediately; duplicate-key conflicts are retried.
func TestAllocateFailures(t *testing.T) {
	t.Run("segment full", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, maxRule([][]any{{int64(1999)}}))
		_, err := NewServer(db, deadRedis()).allocate(context.Background(), seg1000, "")
		if err == nil || !strings.Contains(err.Error(), "full") {
			t.Fatalf("err = %v, want segment full", err)
		}
	})
	t.Run("max query fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "MAX(account_id)", Err: fmt.Errorf("db down")})
		_, err := NewServer(db, deadRedis()).allocate(context.Background(), seg1000, "")
		if err == nil || !strings.Contains(err.Error(), "db down") {
			t.Fatalf("err = %v, want db down", err)
		}
	})
	t.Run("non-duplicate insert fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			maxRule([][]any{{nil}}),
			sqltest.Rule{Contains: "INSERT INTO account_route", Err: fmt.Errorf("conn lost")})
		_, err := NewServer(db, deadRedis()).allocate(context.Background(), seg1000, "")
		if err == nil || !strings.Contains(err.Error(), "conn lost") {
			t.Fatalf("err = %v, want conn lost", err)
		}
	})
}

// allocate retries duplicate-key conflicts with a fresh MAX query each round;
// success on the 5th attempt still yields the correct ID.
func TestAllocateRetriesDuplicateKey(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		maxRuleNil5(),
		sqltest.Rule{Contains: "INSERT INTO account_route", Max: 4, Err: dupEntry()},
		sqltest.Rule{Contains: "INSERT INTO account_route"},
	)
	id, err := NewServer(db, deadRedis()).allocate(context.Background(), seg1000, "")
	if err != nil || id != 1001 {
		t.Fatalf("allocate = %d, %v", id, err)
	}
	if rec.ExecCount("INSERT INTO account_route") != 5 {
		t.Fatalf("insert attempts = %d, want 5", rec.ExecCount("INSERT INTO account_route"))
	}
	if rec.QueryCount("MAX(account_id)") != 5 {
		t.Fatalf("max queries = %d, want 5", rec.QueryCount("MAX(account_id)"))
	}
}

func maxRuleNil5() sqltest.Rule {
	return sqltest.Rule{Contains: "MAX(account_id)", Max: 5,
		Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}}
}

// allocate gives up after 5 duplicate-key rounds.
func TestAllocateRetriesExhausted(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		maxRuleNil5(),
		sqltest.Rule{Contains: "INSERT INTO account_route", Err: dupEntry()},
	)
	_, err := NewServer(db, deadRedis()).allocate(context.Background(), seg1000, "")
	if err == nil || !strings.Contains(err.Error(), "retries") {
		t.Fatalf("err = %v, want retries exhausted", err)
	}
	if rec.ExecCount("INSERT INTO account_route") != 5 {
		t.Fatalf("insert attempts = %d, want 5", rec.ExecCount("INSERT INTO account_route"))
	}
}

// createInDCN: success, non-2xx response and unreachable endpoint.
func TestCreateInDCN(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	s := NewServer(db, deadRedis())
	req := openAccountRequest{Name: "frank", InitBalance: "1.00"}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
	}))
	defer ok.Close()
	if err := s.createInDCN(ok.URL, 1001, req); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	if err := s.createInDCN(bad.URL, 1001, req); err == nil ||
		!strings.Contains(err.Error(), "dcn returned 500") {
		t.Fatalf("err = %v, want dcn returned 500", err)
	}

	if err := s.createInDCN("http://127.0.0.1:1", 1001, req); err == nil {
		t.Fatal("unreachable dcn must fail")
	}
}

// accountCounts: aggregation happy path, query failure and scan failure.
func TestAccountCounts(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "GROUP BY dcn",
			Columns: []string{"dcn", "COUNT(*)"},
			Rows:    [][]any{{"dcn01", int64(2)}, {"dcn02", int64(1)}}})
	got, err := NewServer(db, deadRedis()).accountCounts(context.Background())
	if err != nil || got["dcn01"] != 2 || got["dcn02"] != 1 {
		t.Fatalf("counts = %v, %v", got, err)
	}

	dbQ, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "GROUP BY dcn", Err: fmt.Errorf("db down")})
	if _, err := NewServer(dbQ, deadRedis()).accountCounts(context.Background()); err == nil {
		t.Fatal("query error must propagate")
	}

	// a single column cannot fill the two scan targets → scan error
	dbS, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "GROUP BY dcn",
			Columns: []string{"dcn"}, Rows: [][]any{{"dcn01"}}})
	if _, err := NewServer(dbS, deadRedis()).accountCounts(context.Background()); err == nil {
		t.Fatal("scan error must propagate")
	}
}
