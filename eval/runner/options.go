package runner

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/TamasGorgics/recon-agent/eval/dataset"
	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
	"github.com/TamasGorgics/recon-agent/internal/config"
	"github.com/TamasGorgics/recon-agent/internal/llm"
	"github.com/TamasGorgics/recon-agent/prompts"
)

// Locked defaults from the M3-04 contract.
const (
	DefaultSuite       = "agent"
	DefaultRepeat      = 1
	DefaultConcurrency = 4
	MinConcurrency     = 1
	MaxConcurrency     = 32
	DefaultLLMMode     = "cassette"
	LLMLive            = "live"
	LLMCassette        = "cassette"
	LLMRecord          = "record"
)

// UsageError is a flag or usage problem. The CLI maps it to exit code 2.
type UsageError struct {
	Err error
}

// Error implements error.
func (e *UsageError) Error() string { return e.Err.Error() }

// Unwrap exposes the underlying flag/validation error.
func (e *UsageError) Unwrap() error { return e.Err }

// IsUsage reports whether err is a usage error (exit code 2).
func IsUsage(err error) bool {
	var target *UsageError
	return errors.As(err, &target)
}

func usagef(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// Options are the `recon-eval run` flag values after parsing. A zero field is
// filled by ApplyDefaults; Seed nil means "use the manifest seed".
type Options struct {
	Suite       string
	Split       dataset.Split
	Model       string
	Prompt      string
	Seed        *int64
	Repeat      int
	Concurrency int
	LLM         string
	MaxCostUSD  decimal.Decimal
	Resume      string
	DatasetDir  string
	CassetteDir string

	// UpdateCassettes allows record mode to overwrite an existing cassette.
	// It is rejected for every other LLM mode.
	UpdateCassettes bool

	// The Set fields tell resume whether a flag was explicit. The CLI parser
	// populates them; programmatic callers may leave them false so stored run
	// values win where the stored fingerprint is authoritative. LLMSet,
	// MaxCostUSDSet, and CassetteDirSet are required to distinguish an explicit
	// `--llm cassette`, `--max-cost-usd 0`, or `--cassette-dir` from an omitted
	// flag.
	SuiteSet       bool
	SplitSet       bool
	ModelSet       bool
	PromptSet      bool
	RepeatSet      bool
	ConcurrencySet bool
	LLMSet         bool
	MaxCostUSDSet  bool
	CassetteDirSet bool
}

// ParseOptions parses the `run` subcommand flags. Flag syntax errors and
// ErrHelp are returned unwrapped so the CLI can distinguish help from usage.
func ParseOptions(args []string, out io.Writer) (Options, error) {
	fs := flag.NewFlagSet("recon-eval run", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { _, _ = fmt.Fprint(out, RunUsage) }

	var (
		suite       string
		split       string
		model       string
		prompt      string
		seed        string
		repeat      int
		concurrency int
		llmMode     string
		maxCost     string
		resume      string
		datasetDir  string
		cassetteDir string
		update      bool
	)
	fs.StringVar(&suite, "suite", DefaultSuite, "eval suite (only agent in M3-04)")
	fs.StringVar(&split, "split", string(dataset.Dev), "dataset split: dev, test, or injection")
	fs.StringVar(&model, "model", "", "model API id (default: config default)")
	fs.StringVar(&prompt, "prompt", "", "prompt version (default: config default / v1)")
	fs.StringVar(&seed, "seed", "", "dataset seed; must equal the committed manifest seed")
	fs.IntVar(&repeat, "repeat", DefaultRepeat, "number of linked repeats (>=1)")
	fs.IntVar(&concurrency, "concurrency", DefaultConcurrency, "scenario worker count (1..32)")
	fs.StringVar(&llmMode, "llm", DefaultLLMMode, "LLM mode: live, record, or cassette")
	fs.StringVar(&maxCost, "max-cost-usd", "0", "run cost cap in USD; required > 0 for live/record")
	fs.StringVar(&resume, "resume", "", "existing eval_run id to resume")
	fs.StringVar(&datasetDir, "dataset-dir", "", "dataset directory (default: eval/datasets/<split>)")
	fs.StringVar(&cassetteDir, "cassette-dir", "",
		"cassette root for record/replay (default: eval/cassettes)")
	fs.BoolVar(&update, "update-cassettes", false,
		"with --llm record, overwrite existing cassette files")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if fs.NArg() != 0 {
		return Options{}, usagef("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	opts := Options{
		Suite:           suite,
		Split:           dataset.Split(strings.TrimSpace(split)),
		Model:           strings.TrimSpace(model),
		Prompt:          strings.TrimSpace(prompt),
		Repeat:          repeat,
		Concurrency:     concurrency,
		LLM:             strings.TrimSpace(llmMode),
		Resume:          strings.TrimSpace(resume),
		DatasetDir:      strings.TrimSpace(datasetDir),
		CassetteDir:     strings.TrimSpace(cassetteDir),
		UpdateCassettes: update,
	}
	if seed != "" {
		parsed, err := parseSeed(seed)
		if err != nil {
			return Options{}, err
		}
		opts.Seed = &parsed
	}
	if strings.TrimSpace(maxCost) != "" {
		parsed, err := decimal.NewFromString(strings.TrimSpace(maxCost))
		if err != nil {
			return Options{}, usagef("--max-cost-usd %q is not a decimal", maxCost)
		}
		opts.MaxCostUSD = parsed
	}

	seen := make(map[string]bool, 11)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	opts.SuiteSet = seen["suite"]
	opts.SplitSet = seen["split"]
	opts.ModelSet = seen["model"]
	opts.PromptSet = seen["prompt"]
	opts.RepeatSet = seen["repeat"]
	opts.ConcurrencySet = seen["concurrency"]
	opts.LLMSet = seen["llm"]
	opts.MaxCostUSDSet = seen["max-cost-usd"]
	opts.CassetteDirSet = seen["cassette-dir"]
	return opts, nil
}

// RunUsage is the `recon-eval run` help text.
const RunUsage = `usage: recon-eval run [flags]

Runs the M3 agent eval on one committed dataset split. Results are written to
DATABASE_URL; sandbox databases are disposable. Public LLM modes are live,
record (live execution plus cassette recording), and cassette (replay only);
scripted is an internal test seam only.

Flags:
  --suite string         eval suite (only "agent" in M3-04) (default "agent")
  --split string         dataset split: dev, test, or injection (default "dev")
  --model string         model API id (default: config default)
  --prompt string        prompt version (default: config default / v1)
  --seed int             dataset seed; must equal the committed manifest seed
  --repeat int           linked repeats; each is a separate run (default 1)
  --concurrency int      scenario workers within one run, 1..32 (default 4)
  --llm string           live, record, or cassette (default "cassette")
  --max-cost-usd string  run cost cap; > 0 required for live and record (default "0")
  --cassette-dir string  cassette root for record/replay evidence (default: eval/cassettes)
  --update-cassettes     with --llm record, overwrite existing cassette files
  --resume string        existing eval_run id to resume
  --dataset-dir string   dataset directory (default: eval/datasets/<split>)

Record mode requires OPENCODE_API_KEY in the process environment (never .env,
never a flag), refuses to overwrite an existing cassette unless
--update-cassettes is set, and writes <cassette-dir>/<provider>/<model>/<prompt>/<sha256>.json.

Exit codes:
  0  run completed, or intentionally stopped by the cost cap
  1  validation, database, agent, or scoring infrastructure failure
  2  flag or usage error
`

// ApplyDefaults fills every unspecified option with the locked default. It does
// not validate and does not read the manifest seed.
func ApplyDefaults(opts *Options) {
	opts.Suite = strings.TrimSpace(opts.Suite)
	if opts.Suite == "" && !opts.SuiteSet {
		opts.Suite = DefaultSuite
	}
	if opts.Split == "" && !opts.SplitSet {
		opts.Split = dataset.Dev
	}
	opts.Model = config.NormalizeLLMModel(opts.Model)
	if opts.Model == "" {
		opts.Model = config.DefaultLLMModel
	}
	opts.Prompt = strings.TrimSpace(opts.Prompt)
	if opts.Prompt == "" {
		opts.Prompt = config.DefaultAgentPromptVersion
	}
	opts.Repeat = defaultInt(opts.Repeat, opts.RepeatSet, DefaultRepeat)
	opts.Concurrency = defaultInt(opts.Concurrency, opts.ConcurrencySet, DefaultConcurrency)
	opts.LLM = strings.ToLower(strings.TrimSpace(opts.LLM))
	if opts.LLM == "" {
		opts.LLM = DefaultLLMMode
	}
	opts.CassetteDir = strings.TrimSpace(opts.CassetteDir)
	if opts.CassetteDir == "" {
		opts.CassetteDir = llm.DefaultCassetteDir
	}
}

func defaultInt(value int, set bool, fallback int) int {
	if value == 0 && !set {
		return fallback
	}
	return value
}

// Validate checks every flag-level contract before any database or network
// work. Resume fingerprint checks that need the stored row happen later.
func Validate(opts Options) error {
	if opts.Suite == "" || opts.Suite != DefaultSuite {
		return usagef("--suite %q is not supported; only %q is valid in M3-04", opts.Suite, DefaultSuite)
	}
	if !opts.Split.Valid() {
		return usagef("--split %q is not dev, test, or injection", opts.Split)
	}
	if opts.Model == "" {
		return usagef("--model is required")
	}
	if _, err := llm.DefaultRegistry().Get(opts.Model); err != nil {
		return usagef("--model %q is not in the embedded pricing registry", opts.Model)
	}
	if opts.Prompt == "" {
		return usagef("--prompt is required")
	}
	if _, err := prompts.Load(opts.Prompt); err != nil {
		return usagef("--prompt %q: %v", opts.Prompt, err)
	}
	if opts.Repeat < 1 {
		return usagef("--repeat must be >= 1, got %d", opts.Repeat)
	}
	if opts.Concurrency < MinConcurrency || opts.Concurrency > MaxConcurrency {
		return usagef("--concurrency must be between %d and %d, got %d",
			MinConcurrency, MaxConcurrency, opts.Concurrency)
	}
	if opts.UpdateCassettes && opts.LLM != LLMRecord {
		return usagef("--update-cassettes is only valid with --llm record")
	}
	if opts.LLM == LLMLive && opts.CassetteDirSet {
		return usagef("--cassette-dir is not valid with --llm live; live runs do not read or write cassettes")
	}
	switch opts.LLM {
	case LLMLive, LLMRecord:
		if strings.TrimSpace(os.Getenv(config.EnvOpenCodeAPIKey)) == "" {
			return usagef("--llm %s requires OPENCODE_API_KEY in the process environment", opts.LLM)
		}
		if opts.MaxCostUSD.IsNegative() || opts.MaxCostUSD.IsZero() {
			return usagef("--llm %s requires --max-cost-usd > 0", opts.LLM)
		}
	case LLMCassette:
		// No key and no network. A zero cap means unlimited.
	default:
		return usagef("--llm %q is not live, record, or cassette", opts.LLM)
	}
	if opts.MaxCostUSD.IsNegative() {
		return usagef("--max-cost-usd must not be negative")
	}
	if opts.Resume != "" {
		if !tools.IsUUID(strings.ToLower(opts.Resume)) {
			return usagef("--resume %q is not a UUID", opts.Resume)
		}
		if opts.Repeat != 1 {
			return usagef("--resume requires --repeat 1")
		}
	}
	return nil
}

func parseSeed(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	seed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, usagef("--seed %q is not an integer", raw)
	}
	if seed < 0 {
		return 0, usagef("--seed must not be negative")
	}
	return seed, nil
}
