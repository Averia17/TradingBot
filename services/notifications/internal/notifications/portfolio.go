package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	analyticsTimeout     = 4 * time.Second
	maxAnalyticsResponse = 512 << 10
	positionsPerPage     = 4
	unavailable          = "недоступно"
)

type portfolioReport struct {
	SchemaVersion    string              `json:"schema_version"`
	AccountID        string              `json:"account_id"`
	ID               string              `json:"report_id"`
	Mode             string              `json:"mode"`
	Currency         string              `json:"currency"`
	Status           string              `json:"status"`
	AccountStatus    string              `json:"account_status"`
	AccountVersion   int64               `json:"account_version"`
	AsOf             *time.Time          `json:"as_of"`
	ValuationAt      *time.Time          `json:"valuation_at"`
	LedgerVerifiedAt *time.Time          `json:"ledger_verified_at"`
	Cash             *string             `json:"cash_usd"`
	Reserves         *string             `json:"reserved_cash_usd"`
	CashFree         *string             `json:"free_cash_usd"`
	PositionsValue   *string             `json:"positions_value_usd"`
	NAV              *string             `json:"nav_usd"`
	Realized         *string             `json:"realized_pnl_usd"`
	Unrealized       *string             `json:"unrealized_pnl_usd"`
	Positions        []analyticsPosition `json:"positions"`
	Performance      portfolioPeriod     `json:"performance"`
}

type analyticsPosition struct {
	InstrumentID    string     `json:"instrument_id"`
	RegistryVersion string     `json:"registry_version"`
	Decimals        int        `json:"decimals"`
	Quantity        string     `json:"quantity_raw"`
	Reserved        string     `json:"reserved_quantity_raw"`
	Basis           *string    `json:"basis_usd"`
	Price           *string    `json:"price_usd"`
	MarkObservedAt  *time.Time `json:"mark_observed_at"`
	MarkAvailableAt *time.Time `json:"mark_available_at"`
	MarkSourceID    *string    `json:"mark_source_id"`
	MarkSourceKind  *string    `json:"mark_source_kind"`
	MarketValue     *string    `json:"market_value_usd"`
	Weight          *string    `json:"weight_pct"`
	Unrealized      *string    `json:"unrealized_pnl_usd"`
	Status          string     `json:"status"`
}

type portfolioPeriod struct {
	Period          string     `json:"period"`
	StartAt         *time.Time `json:"start_at"`
	EndAt           *time.Time `json:"end_at"`
	Observations    int        `json:"observations"`
	Status          string     `json:"status"`
	PnL             *string    `json:"pnl_usd"`
	Realized        *string    `json:"realized_pnl_usd"`
	Unrealized      *string    `json:"unrealized_change_usd"`
	Return          *string    `json:"return_pct"`
	BenchmarkID     *string    `json:"benchmark_id"`
	BenchmarkStatus string     `json:"benchmark_status"`
	BenchmarkReturn *string    `json:"benchmark_return_pct"`
	Excess          *string    `json:"excess_pct_points"`
	MaxDrawdown     *string    `json:"max_drawdown_pct"`
	ExecutionFees   *string    `json:"execution_fees_usd"`
	OperatingCosts  *string    `json:"operating_costs_usd"`
	OperatingPnL    *string    `json:"operating_pnl_usd"`
	OperatingReturn *string    `json:"operating_return_pct"`
	OperatingExcess *string    `json:"operating_excess_pct_points"`
	OperatingStatus string     `json:"operating_status"`
}

type portfolioClient struct {
	client   *http.Client
	endpoint url.URL
}

func newPortfolioClient(rawURL string) (*portfolioClient, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" {
		return nil, errors.New("TRADINGBOT_ANALYTICS_URL must contain an HTTP or HTTPS origin")
	}
	endpoint.Path = "/v1/portfolio"
	client := &http.Client{
		Timeout: analyticsTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &portfolioClient{client: client, endpoint: *endpoint}, nil
}

func (c *portfolioClient) fetch(ctx context.Context, period string) (portfolioReport, error) {
	endpoint := c.endpoint
	endpoint.RawQuery = url.Values{"period": {period}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return portfolioReport{}, errors.New("cannot build portfolio report request")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return portfolioReport{}, errors.New("portfolio report transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return portfolioReport{}, fmt.Errorf("portfolio report returned HTTP %d", resp.StatusCode)
	}
	body, err := readBoundedBody(resp.Body, maxAnalyticsResponse)
	if err != nil {
		return portfolioReport{}, errors.New("portfolio report could not be read")
	}
	var report portfolioReport
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil || decoder.Decode(new(any)) != io.EOF || !report.valid(period) {
		return portfolioReport{}, errors.New("portfolio report response was invalid")
	}
	sort.SliceStable(report.Positions, func(i, j int) bool {
		left, right := parseReportNumber(report.Positions[i].MarketValue), parseReportNumber(report.Positions[j].MarketValue)
		return left != nil && (right == nil || left.Cmp(right) > 0)
	})
	return report, nil
}

func (r portfolioReport) valid(period string) bool {
	p := r.Performance
	if r.SchemaVersion != "1.0" || r.Mode != "paper" || r.Currency != "USD" ||
		displayLabel(r.ID) == unavailable || displayLabel(r.AccountID) == unavailable ||
		r.AccountVersion < 0 || p.Period != period || p.Observations < 0 ||
		!validReportNumbers(r.Cash, r.Reserves, r.CashFree, r.PositionsValue, r.NAV, r.Realized, r.Unrealized,
			p.PnL, p.Realized, p.Unrealized, p.Return, p.BenchmarkReturn, p.Excess, p.MaxDrawdown,
			p.ExecutionFees, p.OperatingCosts, p.OperatingPnL, p.OperatingReturn, p.OperatingExcess) {
		return false
	}
	for _, position := range r.Positions {
		if displayLabel(position.InstrumentID) == unavailable || displayLabel(position.RegistryVersion) == unavailable ||
			position.Decimals < 0 || position.Decimals > 30 || !rawQuantity.MatchString(position.Quantity) ||
			!rawQuantity.MatchString(position.Reserved) || !validReportNumbers(
			position.Basis, position.Price, position.MarketValue, position.Weight, position.Unrealized) {
			return false
		}
	}
	return true
}

func validReportNumbers(values ...*string) bool {
	for _, value := range values {
		if value != nil && !reportDecimal.MatchString(*value) {
			return false
		}
	}
	return true
}

func portfolioSummary(report portfolioReport) string {
	var sb strings.Builder
	sb.WriteString(reportHeading("Портфель", report))
	fmt.Fprintf(&sb, "\nСтоимость портфеля: %s\nСвободные деньги: %s\nРезерв денег: %s\nВсего денег: %s\nВ акциях: %s\n",
		money(report.NAV), money(report.CashFree), money(report.Reserves), money(report.Cash), money(report.PositionsValue))
	fmt.Fprintf(&sb, "\nРеализованный P&L счёта: %s\nНереализованный P&L: %s\nP&L за период %s: %s\n",
		money(report.Realized), money(report.Unrealized), report.Performance.Period, money(report.Performance.PnL))
	if len(report.Positions) > 0 {
		sb.WriteString("\nКрупнейшие позиции")
		for _, position := range report.Positions[:min(3, len(report.Positions))] {
			fmt.Fprintf(&sb, "\n%s · %s · %s", displayLabel(position.InstrumentID), money(position.MarketValue), percentage(position.Weight, "%"))
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(reportFooter(report))
	return sb.String()
}

func (r portfolioReport) positionPageCount() int {
	return max(1, (len(r.Positions)+positionsPerPage-1)/positionsPerPage)
}

func (r portfolioReport) hasPage(view string, page int) bool {
	switch view {
	case "positions":
		return page >= 0 && page < r.positionPageCount()
	case "portfolio", "performance":
		return page == 0
	default:
		return false
	}
}

func positionPage(report portfolioReport, page int) string {
	var sb strings.Builder
	sb.WriteString(reportHeading("Позиции", report))
	start := page * positionsPerPage
	if len(report.Positions) == 0 {
		if report.AccountVersion > 0 {
			sb.WriteString("\nОткрытых позиций нет.\n")
		} else {
			sb.WriteString("\nПозиции пока недоступны.\n")
		}
	} else {
		fmt.Fprintf(&sb, "\nСтраница %d из %d\n", page+1, report.positionPageCount())
		for _, p := range report.Positions[start:min(start+positionsPerPage, len(report.Positions))] {
			fmt.Fprintf(&sb, "\n%s · реестр %s\nКоличество: %s · в резерве %s\nСебестоимость FIFO: %s\nЦена: %s · %s\nРыночная стоимость: %s · вес %s\nНереализованный P&L: %s\n",
				displayLabel(p.InstrumentID), displayLabel(p.RegistryVersion),
				positionQuantity(p.Quantity, p.Decimals), positionQuantity(p.Reserved, p.Decimals),
				money(p.Basis), money(p.Price), timestamp(p.MarkObservedAt),
				money(p.MarketValue), percentage(p.Weight, "%"), money(p.Unrealized))
			switch p.Status {
			case "mark_missing":
				sb.WriteString("Цена отсутствует.\n")
			case "mark_stale":
				sb.WriteString("Цена устарела.\n")
			}
		}
	}
	sb.WriteString(reportFooter(report))
	return sb.String()
}

func portfolioPerformance(report portfolioReport) string {
	p := report.Performance
	var sb strings.Builder
	sb.WriteString(reportHeading("Доходность", report))
	fmt.Fprintf(&sb, "\nЗапрошенный период: %s\nДаты оценки: %s — %s\nНаблюдений: %d\n",
		p.Period, timestamp(p.StartAt), timestamp(p.EndAt), p.Observations)
	switch p.Status {
	case "insufficient_history":
		sb.WriteString("Истории пока недостаточно.\n")
	case "incomplete":
		sb.WriteString("Есть пропуски в истории оценки.\n")
	}
	fmt.Fprintf(&sb, "\nP&L портфеля: %s\nРеализованный P&L: %s\nИзменение нереализованного P&L: %s\nДоходность TWR: %s\n",
		money(p.PnL), money(p.Realized), money(p.Unrealized), percentage(p.Return, "%"))
	benchmark := "Бенчмарк"
	if p.BenchmarkID != nil {
		benchmark += " " + displayLabel(*p.BenchmarkID)
	}
	fmt.Fprintf(&sb, "%s: %s\nРазница с бенчмарком: %s\nМаксимальная просадка: %s\n",
		benchmark, percentage(p.BenchmarkReturn, "%"), percentage(p.Excess, " п.п."), percentage(p.MaxDrawdown, "%"))
	fmt.Fprintf(&sb, "Комиссии исполнения: %s\nОперационные расходы: %s\nP&L с операционными расходами: %s\n",
		money(p.ExecutionFees), money(p.OperatingCosts), money(p.OperatingPnL))
	if p.OperatingReturn != nil {
		fmt.Fprintf(&sb, "Доходность с операционными расходами: %s\n", percentage(p.OperatingReturn, "%"))
	}
	if p.OperatingExcess != nil {
		fmt.Fprintf(&sb, "Разница с бенчмарком после расходов: %s\n", percentage(p.OperatingExcess, " п.п."))
	}
	if p.OperatingStatus == "coverage_missing" {
		sb.WriteString("Данные об операционных расходах неполные.\n")
	}
	if p.BenchmarkStatus == "missing" {
		sb.WriteString("Данные бенчмарка неполные.\n")
	}
	sb.WriteString(reportFooter(report))
	return sb.String()
}

func reportHeading(title string, report portfolioReport) string {
	account := map[string]string{
		"current": "сверен", "stale": "сверка устарела", "missing_head": "ожидается сверка", "gap": "пропуск в журнале",
	}
	valuation := map[string]string{
		"complete": "полная", "incomplete": "неполная", "stale": "устарела",
	}
	return fmt.Sprintf("[PAPER · USD] %s\nСостояние счёта: %s · версия %d\nСверка: %s · %s\nОценка: %s · %s\n",
		title, timestamp(report.AsOf), report.AccountVersion, timestamp(report.LedgerVerifiedAt),
		displayLabel(account[report.AccountStatus]), timestamp(report.ValuationAt), displayLabel(valuation[report.Status]))
}

func reportFooter(report portfolioReport) string {
	return "\nID отчёта: " + displayLabel(report.ID)
}

var reportDecimal = regexp.MustCompile(`^-?[0-9]{1,40}(\.[0-9]{1,30})?$`)
var rawQuantity = regexp.MustCompile(`^[0-9]{1,78}$`)

func parseReportNumber(value *string) *big.Rat {
	if value == nil || !reportDecimal.MatchString(*value) {
		return nil
	}
	number, _ := new(big.Rat).SetString(*value)
	return number
}

func money(value *string) string {
	return percentage(value, " USD")
}

func percentage(value *string, unit string) string {
	if value == nil || !reportDecimal.MatchString(*value) {
		return unavailable
	}
	return trimFraction(*value) + unit
}

func positionQuantity(raw string, decimals int) string {
	if !rawQuantity.MatchString(raw) || decimals < 0 || decimals > 30 {
		return unavailable
	}
	return trimFraction(decimalQuantity(raw, decimals))
}

func decimalQuantity(raw string, decimals int) string {
	if decimals == 0 {
		return raw
	}
	if len(raw) <= decimals {
		raw = strings.Repeat("0", decimals+1-len(raw)) + raw
	}
	point := len(raw) - decimals
	return raw[:point] + "." + raw[point:]
}

func trimFraction(value string) string {
	if strings.Contains(value, ".") {
		return strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}
	return value
}

func timestamp(value *time.Time) string {
	if value == nil || value.IsZero() {
		return unavailable
	}
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func displayLabel(value string) string {
	if value == "" || len(value) > 160 || strings.ContainsFunc(value, unicode.IsControl) {
		return unavailable
	}
	return value
}
