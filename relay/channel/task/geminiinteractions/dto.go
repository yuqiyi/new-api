package geminiinteractions

// interactionResponse is the subset of the Gemini Interactions payload this
// adaptor needs for routing, progress and billing. The full body is stored on
// the task so a client polling us receives the upstream shape verbatim.
type interactionResponse struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Upstream execution states. Source: Gemini API background execution docs.
const (
	statusInProgress     = "in_progress"
	statusRequiresAction = "requires_action"
	statusCompleted      = "completed"
	statusFailed         = "failed"
	statusCancelled      = "cancelled"
)

// apiRevisionHeader and apiRevisionValue are required by the Interactions API.
// The client's own value wins when it sends one.
const (
	apiRevisionHeader = "Api-Revision"
	apiRevisionValue  = "2026-05-20"
)
