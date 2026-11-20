package container

// DiagnosticStatus is the result class of a boxr doctor check.
type DiagnosticStatus string

const (
	DiagnosticOK    DiagnosticStatus = "ok"
	DiagnosticWarn  DiagnosticStatus = "warn"
	DiagnosticError DiagnosticStatus = "error"
)

// Diagnostic describes one host prerequisite or runtime limitation.
type Diagnostic struct {
	Name   string
	Status DiagnosticStatus
	Detail string
}
