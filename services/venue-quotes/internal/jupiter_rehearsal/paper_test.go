package jupiter_rehearsal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type paperHarness struct {
	t          *testing.T
	dsn        string
	url        string
	accountID  string
	paperCLI   string
	httpClient *http.Client
}

type paperAccount struct {
	CashUSD         string          `json:"cash_usd"`
	ReservedCashUSD string          `json:"reserved_cash_usd"`
	Version         int64           `json:"version"`
	Positions       []paperPosition `json:"positions"`
}

type paperPosition struct {
	QuantityRaw         string `json:"quantity_raw"`
	ReservedQuantityRaw string `json:"reserved_quantity_raw"`
}

type paperOrder struct {
	OrderID     string    `json:"order_id"`
	QuoteID     string    `json:"quote_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	QuantityRaw string    `json:"quantity_raw"`
	GrossUSD    string    `json:"gross_usd"`
	Side        string    `json:"side"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func startPaper(t *testing.T) *paperHarness {
	t.Helper()
	dsn := startPaperSchema(t)
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancelBuild()
	bins := t.TempDir()
	control := buildPaperBinary(t, buildCtx, bins, "control")
	paperCLI := buildPaperBinary(t, buildCtx, bins, "paper")
	address := startPaperControl(t, control, dsn)
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &paperHarness{
		t: t, dsn: dsn, url: "http://" + address, accountID: newPaperID(t), paperCLI: paperCLI,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func startPaperSchema(t *testing.T) string {
	t.Helper()
	var nonce [12]byte
	raw := os.Getenv("TRADINGBOT_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TRADINGBOT_TEST_DATABASE_URL is required for Jupiter rehearsal")
	}
	uri, err := localPaperDatabaseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGPORT", "PGDATABASE", "PGSERVICE", "PGSERVICEFILE", "PGOPTIONS", "PGTARGETSESSIONATTRS"} {
		if os.Getenv(name) != "" {
			t.Fatalf("clear %s when running the isolated PostgreSQL rehearsal", name)
		}
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate isolated rehearsal schema name")
	}
	schema := "jupiter_rehearsal_" + hex.EncodeToString(nonce[:])
	identifier := pgx.Identifier{schema}.Sanitize()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, uri.String())
	if err != nil {
		t.Fatal("connect to rehearsal PostgreSQL")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.Close(ctx)
	})
	if _, err := db.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal("create isolated rehearsal schema")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error("remove isolated rehearsal schema")
		}
	})
	migration, err := os.ReadFile("../jupiter_execution/001_attempts.sql")
	if err != nil {
		t.Fatal("read Jupiter attempt migration")
	}
	if _, err := db.Exec(ctx, "SET search_path TO "+identifier); err != nil {
		t.Fatal("set search_path to isolated rehearsal schema")
	}
	if _, err := db.Exec(ctx, string(migration)); err != nil {
		t.Fatal("apply Jupiter attempt migration in rehearsal schema")
	}
	params := uri.Query()
	params.Set("search_path", schema)
	uri.RawQuery = params.Encode()
	return uri.String()
}

func localPaperDatabaseURL(raw string) (*url.URL, error) {
	invalid := errors.New("rehearsal requires an explicit loopback PostgreSQL URL without routing overrides")
	uri, err := url.Parse(raw)
	if err != nil || (uri.Scheme != "postgres" && uri.Scheme != "postgresql") || uri.Opaque != "" || uri.Fragment != "" ||
		(uri.Hostname() != "127.0.0.1" && uri.Hostname() != "::1") || uri.User == nil || uri.User.Username() == "" ||
		strings.Count(uri.Path, "/") != 1 || len(uri.Path) < 2 {
		return nil, invalid
	}
	port, err := strconv.ParseUint(uri.Port(), 10, 16)
	if err != nil || port == 0 {
		return nil, invalid
	}
	params, err := url.ParseQuery(uri.RawQuery)
	if err != nil {
		return nil, invalid
	}
	for name, values := range params {
		if name != "sslmode" || len(values) != 1 {
			return nil, invalid
		}
	}
	if params.Get("sslmode") == "" {
		params.Set("sslmode", "disable")
	}
	uri.RawQuery = params.Encode()
	return uri, nil
}

func TestPaperDatabaseURLRequiresOneExplicitLoopback(t *testing.T) {
	for _, raw := range []string{"postgresql://test@127.0.0.1:5432/disposable", "postgres://test@[::1]:5432/disposable?sslmode=disable"} {
		uri, err := localPaperDatabaseURL(raw)
		if err != nil || uri.Query().Get("sslmode") != "disable" {
			t.Fatal("valid loopback test database URL rejected")
		}
	}
	for _, raw := range []string{
		"host=127.0.0.1 port=5432",
		"postgresql://test@localhost:5432/disposable",
		"postgresql://test@127.0.0.1/disposable",
		"postgresql://test@127.0.0.1:5432,192.0.2.1:5432/disposable",
		"postgresql://test@127.0.0.1:5432/disposable?%68ost=192.0.2.1",
		"postgresql://test@127.0.0.1:5432/disposable?service=other",
		"postgresql://test@127.0.0.1:5432/disposable?options=-csearch_path=public",
		"postgresql://test@127.0.0.1:5432/disposable?sslmode=disable&sslmode=require",
	} {
		if _, err := localPaperDatabaseURL(raw); err == nil {
			t.Fatal("test accepted an ambiguous route or routing override")
		}
	}
}

func buildPaperBinary(t *testing.T, ctx context.Context, bins, name string) string {
	t.Helper()
	path := filepath.Join(bins, name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", path, "./cmd/"+name)
	cmd.Dir = filepath.Join("..", "..", "..", "control")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if !strings.HasPrefix(key, "TRADINGBOT_") && !strings.HasPrefix(key, "PG") && key != "GOWORK" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GOWORK=off")
	if err := cmd.Run(); err != nil {
		t.Fatalf("build control workspace command %s failed", name)
	}
	return path
}

func paperRuntimeEnv(dsn string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR":
			env = append(env, entry)
		}
	}
	return append(env, "TRADINGBOT_DATABASE_URL="+dsn)
}

func startPaperControl(t *testing.T, binary, dsn string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, "-listen", "127.0.0.1:0")
	cmd.Env = paperRuntimeEnv(dsn)
	stderr, err := cmd.StderrPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("start local paper control API")
	}
	done := make(chan struct{})
	ready := make(chan string, 1)
	go func() {
		defer close(done)
		defer close(ready)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			_, address, found := strings.Cut(line, " address=")
			if !found || !strings.Contains(line, "research control listening") {
				continue
			}
			host, port, err := net.SplitHostPort(address)
			if err == nil && host == "127.0.0.1" && port != "0" {
				select {
				case ready <- address:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		_ = cmd.Wait()
	})
	select {
	case address := <-ready:
		if address == "" {
			t.Fatal("local paper control stopped during startup")
		}
		return address
	case <-time.After(30 * time.Second):
		t.Fatal("local paper control did not report loopback readiness")
	case <-t.Context().Done():
		t.Fatal("rehearsal canceled while starting paper control")
	}
	return ""
}

func (p *paperHarness) create() paperAccount {
	p.t.Helper()
	var account paperAccount
	p.request(http.MethodPost, "/v1/paper/accounts", http.StatusCreated,
		map[string]string{"account_id": p.accountID, "initial_cash_usd": "1000"}, &account)
	return account
}

func (p *paperHarness) account() paperAccount {
	p.t.Helper()
	var account paperAccount
	p.request(http.MethodGet, p.accountPath(), http.StatusOK, nil, &account)
	return account
}

func (p *paperHarness) lookup(orderID string) paperOrder {
	p.t.Helper()
	var order paperOrder
	p.request(http.MethodGet, p.accountPath()+"/orders/"+orderID, http.StatusOK, nil, &order)
	return order
}

func (p *paperHarness) reserve(side, qtyRaw, grossUSD string) paperOrder {
	p.t.Helper()
	var order paperOrder
	p.request(http.MethodPost, p.accountPath()+"/orders", http.StatusCreated, map[string]any{
		"account_id": p.accountID, "order_id": newPaperID(p.t), "decision_id": newPaperID(p.t),
		"quote_id": newPaperID(p.t), "snapshot_version": p.account().Version,
		"instrument": map[string]any{"instrument_id": "fixture:Jupiter", "registry_version": "fixture-v1", "decimals": 0},
		"side":       side, "quantity_raw": qtyRaw, "gross_usd": grossUSD, "max_fee_usd": "0",
		"expires_at": time.Now().UTC().Truncate(time.Microsecond).Add(5 * time.Minute),
	}, &order)
	return order
}

func (p *paperHarness) markUnknown(order paperOrder) paperOrder {
	p.t.Helper()
	return p.resolve(order.OrderID, map[string]any{
		"account_id": p.accountID, "order_id": order.OrderID, "event_id": newPaperID(p.t), "outcome": "unknown",
	})
}

func (p *paperHarness) settle(order paperOrder, qtyRaw, grossUSD string, fromUnknown bool) paperOrder {
	p.t.Helper()
	// This fixture values USDC at par and keeps synthetic SOL fees separate.
	// Both attempts use identical bytes and event identity.
	observed := time.Now().UTC().Truncate(time.Microsecond)
	eventID := newPaperID(p.t)
	var settled, replay paperOrder
	if fromUnknown {
		observation := map[string]any{
			"paper_observation_schema_version": "0.1", "source_mode": "synthetic_fixture",
			"account_id": p.accountID, "order_id": order.OrderID, "quote_id": order.QuoteID,
			"observation_id": eventID, "outcome": "filled", "observed_at": observed, "received_at": observed,
			"quantity_raw": qtyRaw, "gross_usd": grossUSD, "fee_usd": "0",
		}
		raw, err := json.Marshal(observation)
		path := filepath.Join(p.t.TempDir(), "paper_observation.json")
		if err != nil || os.WriteFile(path, raw, 0600) != nil {
			p.t.Fatal("write synthetic reconciliation observation")
		}
		digest := sha256.Sum256(raw)
		settled = p.reconcile(path, hex.EncodeToString(digest[:]), true)
		after := p.account()
		replay = p.reconcile(path, hex.EncodeToString(digest[:]), false)
		p.checkSettlementReplay(settled, replay, after)
	} else {
		resolution := map[string]any{
			"account_id": p.accountID, "order_id": order.OrderID, "event_id": eventID, "outcome": "filled",
			"quantity_raw": qtyRaw, "gross_usd": grossUSD, "fee_usd": "0", "observed_at": observed,
		}
		settled = p.resolve(order.OrderID, resolution)
		after := p.account()
		replay = p.resolve(order.OrderID, resolution)
		p.checkSettlementReplay(settled, replay, after)
	}
	return settled
}

func (p *paperHarness) checkSettlementReplay(settled, replay paperOrder, account paperAccount) {
	p.t.Helper()
	if settled.Status != "filled" || !reflect.DeepEqual(settled, replay) || !reflect.DeepEqual(account, p.account()) {
		p.t.Fatal("paper settlement was not a stable, idempotent fill")
	}
}

func (p *paperHarness) reconcile(path, digest string, recorded bool) paperOrder {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.paperCLI, "reconcile", "--observation", path, "--sha256", digest)
	cmd.Env = paperRuntimeEnv(p.dsn)
	raw, err := cmd.Output()
	var receipt struct {
		SHA256   string     `json:"observation_sha256"`
		Recorded bool       `json:"recorded"`
		Order    paperOrder `json:"order"`
	}
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.SHA256 != digest || receipt.Recorded != recorded {
		p.t.Fatal("paper reconcile did not return the expected verified receipt")
	}
	return receipt.Order
}

func (p *paperHarness) resolve(orderID string, payload map[string]any) paperOrder {
	p.t.Helper()
	var order paperOrder
	p.request(http.MethodPost, p.accountPath()+"/orders/"+orderID+"/resolve", http.StatusOK, payload, &order)
	return order
}

func (p *paperHarness) accountPath() string {
	return "/v1/paper/accounts/" + p.accountID
}

func (p *paperHarness) request(method, path string, expected int, payload, target any) {
	p.t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			p.t.Fatal("encode paper control request")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(p.t.Context(), method, p.url+path, body)
	if err != nil {
		p.t.Fatal("create paper control request")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.httpClient.Do(request)
	if err != nil {
		p.t.Fatal("local paper control request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		p.t.Fatalf("paper control %s returned HTTP %d, expected %d", method, response.StatusCode, expected)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(target); err != nil {
		p.t.Fatal("decode local paper control response")
	}
}

func newPaperID(t *testing.T) string {
	t.Helper()
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		t.Fatal("generate rehearsal ID")
	}
	r[6] = (r[6] & 0x0f) | 0x40
	r[8] = (r[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", r[:4], r[4:6], r[6:8], r[8:10], r[10:])
}
