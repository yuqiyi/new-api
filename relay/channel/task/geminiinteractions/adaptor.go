package geminiinteractions

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	geminichannel "github.com/QuantumNous/new-api/relay/channel/gemini"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
)

// TaskAdaptor relays Gemini background interactions
// (POST /v1beta/interactions with "background": true) through the async task
// pipeline. Synchronous interactions keep using GeminiInteractionsHelper.
type TaskAdaptor struct {
	ChannelType int
	apiKey      string
	baseURL     string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	a.ChannelType = info.ChannelType
	a.baseURL = info.ChannelBaseUrl
	a.apiKey = info.ApiKey
}

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.TaskError {
	request, err := parseInteractionsRequest(c)
	if err != nil {
		return service.TaskErrorWrapperLocal(err, "invalid_request", http.StatusBadRequest)
	}
	if request.Model == "" {
		return service.TaskErrorWrapperLocal(fmt.Errorf("model is required"), "invalid_request", http.StatusBadRequest)
	}
	info.OriginModelName = request.Model
	info.UpstreamModelName = request.Model
	info.Action = constant.TaskActionBackground
	return nil
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	version := model_setting.GetGeminiVersionSetting(info.UpstreamModelName)
	return fmt.Sprintf("%s/%s/interactions", a.baseURL, version), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-goog-api-key", a.apiKey)
	// The Interactions API is revision-pinned. Forward the client's revision so
	// it keeps control of the contract, and fall back to the documented one.
	if revision := c.Request.Header.Get(apiRevisionHeader); revision != "" {
		req.Header.Set(apiRevisionHeader, revision)
	} else {
		req.Header.Set(apiRevisionHeader, apiRevisionValue)
	}
	return nil
}

// BuildRequestBody forwards the client payload unchanged apart from the mapped
// model name. The Interactions API accepts evolving multimodal input types, so
// rewriting the body would drop fields this gateway does not know yet.
func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	request, err := parseInteractionsRequest(c)
	if err != nil {
		return nil, err
	}
	request.SetModelName(info.UpstreamModelName)
	jsonData, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(info.ParamOverride) > 0 {
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return nil, err
		}
	}
	body, _, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	return channel.DoTaskApiRequest(a, c, info, requestBody)
}

// DoResponse returns the upstream interaction id as the task id so a client can
// keep using the official polling path GET /v1beta/interactions/{id}.
//
// It also overrides info.PublicTaskID with that same id. RelayTaskSubmit
// pre-generates a task_xxxx public id, but the response body forwarded to the
// client carries the upstream id verbatim — leaving the two different would
// store a row the client can never look up. InitTask reads PublicTaskID after
// this returns, so the override makes the stored task id, the task-log id and
// the id in the response body one and the same value.
func (a *TaskAdaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (string, []byte, *taskdto.TaskError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, service.TaskErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	var interaction interactionResponse
	if err := common.Unmarshal(responseBody, &interaction); err != nil {
		return "", nil, service.TaskErrorWrapper(err, "unmarshal_response_failed", http.StatusInternalServerError)
	}
	if interaction.ID == "" {
		return "", nil, service.TaskErrorWrapper(fmt.Errorf("upstream returned no interaction id"),
			"invalid_upstream_response", http.StatusInternalServerError)
	}

	info.PublicTaskID = interaction.ID

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(responseBody)

	return interaction.ID, responseBody, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return []string{"gemini-omni-flash-preview"}
}

func (a *TaskAdaptor) GetChannelName() string {
	return "gemini_interactions"
}

// FetchTask polls GET /v1beta/interactions/{id}. The id is stored verbatim, so
// no decoding step is needed here.
func (a *TaskAdaptor) FetchTask(baseUrl, key string, body map[string]any, proxy string) (*http.Response, error) {
	interactionID, ok := body["task_id"].(string)
	if !ok || interactionID == "" {
		return nil, fmt.Errorf("invalid task_id")
	}

	version := model_setting.GetGeminiVersionSetting("default")
	url := fmt.Sprintf("%s/%s/interactions/%s", baseUrl, version, interactionID)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-goog-api-key", key)
	req.Header.Set(apiRevisionHeader, apiRevisionValue)

	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(respBody []byte) (*relaycommon.TaskInfo, error) {
	var interaction interactionResponse
	if err := common.Unmarshal(respBody, &interaction); err != nil {
		return nil, fmt.Errorf("unmarshal interaction response failed: %w", err)
	}

	ti := &relaycommon.TaskInfo{TaskID: interaction.ID}

	switch interaction.Status {
	case statusCompleted:
		ti.Status = model.TaskStatusSuccess
		ti.Progress = taskcommon.ProgressComplete
	case statusFailed, statusCancelled:
		ti.Status = model.TaskStatusFailure
		ti.Progress = taskcommon.ProgressComplete
		ti.Reason = fmt.Sprintf("upstream interaction %s", interaction.Status)
		if interaction.Error != nil && interaction.Error.Message != "" {
			ti.Reason = interaction.Error.Message
		}
	case statusInProgress, statusRequiresAction:
		// requires_action means the upstream awaits client input. Video
		// generation never reaches it; keeping the task in progress lets the
		// timeout sweeper refund instead of failing a live agent run early.
		ti.Status = model.TaskStatusInProgress
		ti.Progress = taskcommon.ProgressInProgress
	default:
		if interaction.Error != nil && interaction.Error.Message != "" {
			ti.Status = model.TaskStatusFailure
			ti.Progress = taskcommon.ProgressComplete
			ti.Reason = interaction.Error.Message
			return ti, nil
		}
		// Unknown status: stay in progress rather than settling on a guess.
		ti.Status = model.TaskStatusInProgress
		ti.Progress = taskcommon.ProgressInProgress
	}

	if usage := usageFromInteractionPayload(respBody); usage != nil {
		ti.TotalTokens = usage.TotalTokens
		ti.CompletionTokens = usage.CompletionTokens
	}
	return ti, nil
}

// ── Billing ──────────────────────────────────────────────────────────────

// EstimateBilling returns nil: the interactions payload is free-form, so there
// is no duration or resolution to derive a pre-charge multiplier from. The
// pre-charge stays at the model's base estimate and AdjustBillingOnComplete
// settles the difference once real usage is known.
func (a *TaskAdaptor) EstimateBilling(_ *gin.Context, _ *relaycommon.RelayInfo) map[string]float64 {
	return nil
}

func (a *TaskAdaptor) AdjustBillingOnSubmit(_ *relaycommon.RelayInfo, _ []byte) map[string]float64 {
	return nil
}

// AdjustBillingOnComplete charges the completed interaction with the same
// modality-aware arithmetic the synchronous path uses. The background response
// carries the identical `usage` object, so video output is billed at
// VideoOutputRatio instead of collapsing into the blended text rate.
func (a *TaskAdaptor) AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int {
	quota, _ := a.settleCompleted(task, taskResult)
	return quota
}

// ExplainBillingOnComplete reports the token breakdown and ratios behind the
// settled quota so the task log can show the same arithmetic a synchronous
// request displays. Implements service.AsyncBillingExplainer.
func (a *TaskAdaptor) ExplainBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) service.AsyncBillingBreakdown {
	_, breakdown := a.settleCompleted(task, taskResult)
	return breakdown
}

func (a *TaskAdaptor) settleCompleted(task *model.Task, taskResult *relaycommon.TaskInfo) (int, service.AsyncBillingBreakdown) {
	if task == nil || taskResult == nil || taskResult.Status != model.TaskStatusSuccess {
		return 0, nil
	}
	usage := usageFromInteractionPayload(task.Data)
	if usage == nil || usage.TotalTokens == 0 {
		return 0, nil
	}

	modelName := task.Properties.OriginModelName
	groupRatio := 1.0
	var otherRatios map[string]float64
	if bc := task.PrivateData.BillingContext; bc != nil {
		if bc.OriginModelName != "" {
			modelName = bc.OriginModelName
		}
		if bc.GroupRatio > 0 {
			groupRatio = bc.GroupRatio
		}
		otherRatios = bc.OtherRatios
	}
	if modelName == "" {
		return 0, nil
	}

	quota, clamp, breakdown := service.SettleAsyncInteractionQuotaDetailed(modelName, groupRatio, usage, otherRatios)
	if clamp != nil {
		// The conversion helper already records the clamp via common.SysError;
		// this adds the task id. Settlement runs in the polling sweep, which has
		// no request context, so SYSTEM attribution is the correct one.
		logger.LogWarn(context.Background(),
			fmt.Sprintf("gemini interaction %s quota saturated during settlement", task.TaskID))
	}
	return quota, breakdown
}

// usageFromInteractionPayload reuses the synchronous usage parser so both paths
// agree on token attribution, including the per-modality arrays.
func usageFromInteractionPayload(payload []byte) *dto.Usage {
	if len(payload) == 0 {
		return nil
	}
	return geminichannel.ParseInteractionsUsage(payload)
}

// parseInteractionsRequest reads the client payload from the replayable body so
// both validation and body building see the same parsed request.
func parseInteractionsRequest(c *gin.Context) (*dto.GeminiInteractionsRequest, error) {
	request := &dto.GeminiInteractionsRequest{}
	if err := common.UnmarshalBodyReusable(c, request); err != nil {
		return nil, err
	}
	return request, nil
}
