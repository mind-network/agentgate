package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentgate/internal/lp/gwclient"
)

// StatsOptions holds parsed CLI options for aicg stats.
type StatsOptions struct {
	Dim  string // team, user, repo, model
	From string // YYYY-MM-DD (inclusive)
	To   string // YYYY-MM-DD (exclusive)
}

// ParseStatsOptions parses CLI args into StatsOptions using the current wall
// clock as the time anchor.
func ParseStatsOptions(args []string) (StatsOptions, error) {
	return ParseStatsOptionsAt(args, time.Now().UTC())
}

// ParseStatsOptionsAt parses CLI args into StatsOptions using the supplied
// `now` value as the time anchor. Tests inject a fixed `now` to avoid wall
// clock dependency.
func ParseStatsOptionsAt(args []string, now time.Time) (StatsOptions, error) {
	opts := StatsOptions{
		Dim: "team",
	}

	var (
		periodSet      bool
		fromGiven      bool
		fromIsRelative bool
		toGiven        bool
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--today":
			if periodSet || fromGiven || toGiven {
				return StatsOptions{}, fmt.Errorf("conflicting period flags specified")
			}
			periodSet = true
			opts.From = now.Format("2006-01-02")
			opts.To = now.Add(24 * time.Hour).Format("2006-01-02")

		case "--this-month":
			if periodSet || fromGiven || toGiven {
				return StatsOptions{}, fmt.Errorf("conflicting period flags specified")
			}
			periodSet = true
			opts.From = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
			opts.To = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")

		case "--from":
			if periodSet {
				return StatsOptions{}, fmt.Errorf("conflicting period flags specified")
			}
			if fromGiven {
				return StatsOptions{}, fmt.Errorf("--from given more than once")
			}
			if i+1 >= len(args) {
				return StatsOptions{}, fmt.Errorf("--from requires a value (Nd, Nw, or YYYY-MM-DD)")
			}
			val := args[i+1]
			fromVal, toVal, isRel, perr := parseFromValue(val, now)
			if perr != nil {
				return StatsOptions{}, perr
			}
			opts.From = fromVal
			if isRel && !toGiven {
				opts.To = toVal
			}
			fromGiven = true
			fromIsRelative = isRel
			i++

		case "--to":
			if periodSet {
				return StatsOptions{}, fmt.Errorf("conflicting period flags specified")
			}
			if toGiven {
				return StatsOptions{}, fmt.Errorf("--to given more than once")
			}
			if i+1 >= len(args) {
				return StatsOptions{}, fmt.Errorf("--to requires a value (YYYY-MM-DD)")
			}
			val := args[i+1]
			if _, perr := time.Parse("2006-01-02", val); perr != nil {
				return StatsOptions{}, fmt.Errorf("--to: must be YYYY-MM-DD (got %q)", val)
			}
			opts.To = val
			toGiven = true
			i++

		case "--by":
			if i+1 >= len(args) {
				return StatsOptions{}, fmt.Errorf("--by requires a dimension value (team, user, repo, model)")
			}
			opts.Dim = strings.ToLower(args[i+1])
			// Validate immediately so the error is about the value, not an unknown flag.
			switch opts.Dim {
			case "team", "user", "repo", "model":
				// valid
			default:
				return StatsOptions{}, fmt.Errorf("invalid dimension %q (must be team, user, repo, or model)", opts.Dim)
			}
			i++

		case "--by-team":
			opts.Dim = "team"
		case "--by-user":
			opts.Dim = "user"
		case "--by-repo":
			opts.Dim = "repo"
		case "--by-model":
			opts.Dim = "model"

		default:
			return StatsOptions{}, fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	// --to without --from is invalid.
	if toGiven && !fromGiven {
		return StatsOptions{}, fmt.Errorf("--to requires --from")
	}

	// Explicit ISO --from without --to: default To = tomorrow_UTC.
	if fromGiven && !fromIsRelative && !toGiven {
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		opts.To = today.AddDate(0, 0, 1).Format("2006-01-02")
	}

	// Default to this-month when no period flag is given at all.
	if !periodSet && !fromGiven && !toGiven {
		opts.From = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		opts.To = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	}

	return opts, nil
}

// parseFromValue parses the value passed to --from. It accepts:
//   - a relative duration `Nd` or `Nw` (N positive integer) → returns the
//     YYYY-MM-DD range covering N calendar days ending today, with toStr =
//     tomorrow_UTC; isRelative=true.
//   - an explicit YYYY-MM-DD date → returns fromStr=s, toStr="" (the caller
//     fills in --to or defaults to tomorrow); isRelative=false.
//
// Any other input — non-integer numbers, zero or negative, unsupported units
// (h / m / s / mo / …), or malformed dates — returns the uniform error
// "--from: must be Nd, Nw, or YYYY-MM-DD (got %q)".
func parseFromValue(s string, now time.Time) (fromStr, toStr string, isRelative bool, err error) {
	invalid := fmt.Errorf("--from: must be Nd, Nw, or YYYY-MM-DD (got %q)", s)
	if s == "" {
		return "", "", false, invalid
	}
	// Explicit YYYY-MM-DD.
	if _, perr := time.Parse("2006-01-02", s); perr == nil {
		return s, "", false, nil
	}
	// Relative duration Nd / Nw.
	if len(s) < 2 {
		return "", "", false, invalid
	}
	suffix := s[len(s)-1]
	if suffix != 'd' && suffix != 'w' {
		return "", "", false, invalid
	}
	n, nerr := strconv.Atoi(s[:len(s)-1])
	if nerr != nil {
		return "", "", false, invalid
	}
	if n <= 0 {
		return "", "", false, invalid
	}
	days := n
	if suffix == 'w' {
		days = n * 7
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fromTime := today.AddDate(0, 0, -(days - 1))
	toTime := today.AddDate(0, 0, 1)
	return fromTime.Format("2006-01-02"), toTime.Format("2006-01-02"), true, nil
}

// RenderStatsTable renders cost summary rows as a human-readable ASCII table.
func RenderStatsTable(opts StatsOptions, rows []gwclient.CostSummaryRow) string {
	var b strings.Builder

	dimLabel := "by " + opts.Dim
	fmt.Fprintf(&b, "Cost Summary  (%s – %s  ·  %s)\n", opts.From, opts.To, dimLabel)

	if len(rows) == 0 {
		fmt.Fprintf(&b, "  (no data for this period)\n")
		return b.String()
	}

	// Sort by cost descending, then dim_value ascending for deterministic output.
	sorted := make([]gwclient.CostSummaryRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CostCents != sorted[j].CostCents {
			return sorted[i].CostCents > sorted[j].CostCents
		}
		return sorted[i].DimValue < sorted[j].DimValue
	})

	fmt.Fprintf(&b, "  %-20s %8s %8s %8s %8s %8s\n",
		"dim_value", "requests", "tok_in", "tok_out", "failed", "cost")
	for _, row := range sorted {
		fmt.Fprintf(&b, "  %-20s %8d %8d %8d %8d %8s\n",
			truncate(row.DimValue, 20),
			row.NRequests, row.InputTokens, row.OutputTokens,
			row.NFailed, formatCents(row.CostCents))
	}
	return b.String()
}

// formatCents formats an integer cent amount as a dollar string (e.g. 1250 → "12.50").
func formatCents(cents int) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	dollars := cents / 100
	remain := cents % 100
	return fmt.Sprintf("%s%d.%02d", sign, dollars, remain)
}
