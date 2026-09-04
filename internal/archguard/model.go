package archguard

import "regexp"

type ViolationKind string

const (
	KindImport                ViolationKind = "import"
	KindMutableGlobal         ViolationKind = "mutable_global"
	KindAPIConstruction       ViolationKind = "api_construction"
	KindProductionTestHook    ViolationKind = "production_test_hook"
	KindFileBudget            ViolationKind = "file_budget"
	KindHistoryComment        ViolationKind = "history_comment"
	KindEmbeddedDocument      ViolationKind = "embedded_document"
	KindWorker                ViolationKind = "worker"
	KindGenericPackage        ViolationKind = "generic_package"
	KindProductionConstructor ViolationKind = "production_constructor"
)

type Location struct {
	Path   string `json:"path" yaml:"path"`
	Symbol string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
	Line   int    `json:"line,omitempty" yaml:"line,omitempty"`
}

type Violation struct {
	Fingerprint string        `json:"fingerprint" yaml:"fingerprint"`
	Kind        ViolationKind `json:"kind" yaml:"kind"`
	Location    Location      `json:"location" yaml:"location"`
	Evidence    string        `json:"evidence" yaml:"evidence"`
	Metric      int           `json:"metric,omitempty" yaml:"metric,omitempty"`
	Message     string        `json:"message" yaml:"message"`
}

type Report struct {
	SchemaVersion int                   `json:"schema_version" yaml:"schema_version"`
	Module        string                `json:"module" yaml:"module"`
	Root          string                `json:"root" yaml:"root"`
	Violations    []Violation           `json:"violations" yaml:"violations"`
	Summary       map[ViolationKind]int `json:"summary" yaml:"summary"`
}

type FileBudgets struct {
	ProductionAdvisoryLines int `json:"production_advisory_lines" yaml:"production_advisory_lines"`
	ProductionHardLines     int `json:"production_hard_lines" yaml:"production_hard_lines"`
	TestReviewLines         int `json:"test_review_lines" yaml:"test_review_lines"`
}

type ImportRule struct {
	From []string `json:"from" yaml:"from"`
	Deny []string `json:"deny" yaml:"deny"`
}

type SymbolRule struct {
	ImportPath   string   `json:"import_path" yaml:"import_path"`
	Symbol       string   `json:"symbol" yaml:"symbol"`
	AllowedGlobs []string `json:"allowed_globs" yaml:"allowed_globs"`
}

type Policy struct {
	SchemaVersion             int          `json:"schema_version" yaml:"schema_version"`
	Module                    string       `json:"module" yaml:"module"`
	SourceRoots               []string     `json:"source_roots" yaml:"source_roots"`
	ExcludeGlobs              []string     `json:"exclude_globs" yaml:"exclude_globs"`
	ImportRules               []ImportRule `json:"import_rules" yaml:"import_rules"`
	APIConstructors           []SymbolRule `json:"api_constructors" yaml:"api_constructors"`
	ProductionConstructors    []SymbolRule `json:"production_constructors" yaml:"production_constructors"`
	AllowedGlobalNamePatterns []string     `json:"allowed_global_name_patterns" yaml:"allowed_global_name_patterns"`
	TestHookNamePatterns      []string     `json:"test_hook_name_patterns" yaml:"test_hook_name_patterns"`
	HistoryCommentPatterns    []string     `json:"history_comment_patterns" yaml:"history_comment_patterns"`
	HistoryAllowedGlobs       []string     `json:"history_allowed_globs" yaml:"history_allowed_globs"`
	TestOnlyBuildTags         []string     `json:"test_only_build_tags" yaml:"test_only_build_tags"`
	EmbeddedDocumentMinBytes  int          `json:"embedded_document_min_bytes" yaml:"embedded_document_min_bytes"`
	FileBudgets               FileBudgets  `json:"file_budgets" yaml:"file_budgets"`
	GenericPackageNames       []string     `json:"generic_package_names" yaml:"generic_package_names"`

	compiledAllowedGlobals []*regexp.Regexp
	compiledTestHooks      []*regexp.Regexp
	compiledHistory        []*regexp.Regexp
}

type BaselineEntry struct {
	Violation   `yaml:",inline"`
	Owner       string `json:"owner" yaml:"owner"`
	WorkPackage string `json:"work_package" yaml:"work_package"`
	RemoveBy    string `json:"remove_by" yaml:"remove_by"`
	Reason      string `json:"reason" yaml:"reason"`
	MaxMetric   int    `json:"max_metric,omitempty" yaml:"max_metric,omitempty"`
}

type Baseline struct {
	SchemaVersion int             `json:"schema_version" yaml:"schema_version"`
	GeneratedFrom string          `json:"generated_from" yaml:"generated_from"`
	Entries       []BaselineEntry `json:"entries" yaml:"entries"`
}

type WorkerRecord struct {
	Fingerprint  string `json:"fingerprint" yaml:"fingerprint"`
	Component    string `json:"component" yaml:"component"`
	Owner        string `json:"owner" yaml:"owner"`
	Start        string `json:"start" yaml:"start"`
	Cancel       string `json:"cancel" yaml:"cancel"`
	Join         string `json:"join" yaml:"join"`
	BoundedBy    string `json:"bounded_by" yaml:"bounded_by"`
	ContractTest string `json:"contract_test" yaml:"contract_test"`
	WorkPackage  string `json:"work_package" yaml:"work_package"`
	RemoveBy     string `json:"remove_by,omitempty" yaml:"remove_by,omitempty"`
	Reason       string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type Workers struct {
	SchemaVersion int            `json:"schema_version" yaml:"schema_version"`
	Entries       []WorkerRecord `json:"entries" yaml:"entries"`
}

type OwnerRule struct {
	Globs       []string        `json:"globs" yaml:"globs"`
	Kinds       []ViolationKind `json:"kinds,omitempty" yaml:"kinds,omitempty"`
	Owner       string          `json:"owner" yaml:"owner"`
	WorkPackage string          `json:"work_package" yaml:"work_package"`
	RemoveBy    string          `json:"remove_by" yaml:"remove_by"`
	Reason      string          `json:"reason" yaml:"reason"`
}

type Owners struct {
	SchemaVersion int         `json:"schema_version" yaml:"schema_version"`
	Rules         []OwnerRule `json:"rules" yaml:"rules"`
}

type MetricChange struct {
	Violation Violation     `json:"violation" yaml:"violation"`
	Baseline  BaselineEntry `json:"baseline" yaml:"baseline"`
}

type WorkerProblem struct {
	Fingerprint string `json:"fingerprint" yaml:"fingerprint"`
	Problem     string `json:"problem" yaml:"problem"`
}

type Verification struct {
	New     []Violation     `json:"new" yaml:"new"`
	Grown   []MetricChange  `json:"grown" yaml:"grown"`
	Expired []BaselineEntry `json:"expired" yaml:"expired"`
	Stale   []BaselineEntry `json:"stale" yaml:"stale"`
	Unowned []BaselineEntry `json:"unowned" yaml:"unowned"`
	Workers []WorkerProblem `json:"workers" yaml:"workers"`
}
