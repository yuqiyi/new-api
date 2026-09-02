package geminiinteractions

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	taskcommon "github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseTaskResultMapsUpstreamStatus locks the mapping from the Interactions
// execution states onto task status. Getting this wrong either refunds a running
// task or leaves a finished one spinning until the timeout sweeper kills it.
func TestParseTaskResultMapsUpstreamStatus(t *testing.T) {
	cases := []struct {
		name         string
		payload      string
		wantStatus   string
		wantProgress string
		wantReason   string
	}{
		{
			name:         "completed",
			payload:      `{"id":"v1_abc","status":"completed"}`,
			wantStatus:   model.TaskStatusSuccess,
			wantProgress: taskcommon.ProgressComplete,
		},
		{
			name:         "in_progress",
			payload:      `{"id":"v1_abc","status":"in_progress"}`,
			wantStatus:   model.TaskStatusInProgress,
			wantProgress: taskcommon.ProgressInProgress,
		},
		{
			name:         "requires_action stays in progress",
			payload:      `{"id":"v1_abc","status":"requires_action"}`,
			wantStatus:   model.TaskStatusInProgress,
			wantProgress: taskcommon.ProgressInProgress,
		},
		{
			name:         "failed carries upstream message",
			payload:      `{"id":"v1_abc","status":"failed","error":{"message":"quota exceeded"}}`,
			wantStatus:   model.TaskStatusFailure,
			wantProgress: taskcommon.ProgressComplete,
			wantReason:   "quota exceeded",
		},
		{
			name:         "cancelled is terminal",
			payload:      `{"id":"v1_abc","status":"cancelled"}`,
			wantStatus:   model.TaskStatusFailure,
			wantProgress: taskcommon.ProgressComplete,
			wantReason:   "upstream interaction cancelled",
		},
		{
			name:         "unknown status does not settle",
			payload:      `{"id":"v1_abc","status":"something_new"}`,
			wantStatus:   model.TaskStatusInProgress,
			wantProgress: taskcommon.ProgressInProgress,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ti, err := (&TaskAdaptor{}).ParseTaskResult([]byte(tc.payload))
			require.NoError(t, err)
			require.NotNil(t, ti)
			assert.Equal(t, tc.wantStatus, ti.Status)
			assert.Equal(t, tc.wantProgress, ti.Progress)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, ti.Reason)
			}
			assert.Equal(t, "v1_abc", ti.TaskID)
		})
	}
}

// TestParseTaskResultReadsModalityUsage covers the real completion payload: the
// per-modality arrays must reach TaskInfo so settlement can bill video output
// separately instead of at the blended text rate.
func TestParseTaskResultReadsModalityUsage(t *testing.T) {
	payload := []byte(`{
		"id": "v1_abc",
		"status": "completed",
		"usage": {
			"total_tokens": 59244,
			"total_input_tokens": 13,
			"total_output_tokens": 58865,
			"total_thought_tokens": 366,
			"total_cached_tokens": 0,
			"input_tokens_by_modality": [{"modality": "text", "tokens": 13}],
			"output_tokens_by_modality": [{"modality": "video", "tokens": 57920}]
		}
	}`)

	ti, err := (&TaskAdaptor{}).ParseTaskResult(payload)
	require.NoError(t, err)
	require.NotNil(t, ti)
	assert.Equal(t, model.TaskStatusSuccess, ti.Status)
	assert.Equal(t, 59244, ti.TotalTokens)
	assert.Equal(t, 58865, ti.CompletionTokens)
}

// TestAdjustBillingOnCompleteOnlySettlesSuccess guards the billing invariant:
// a non-terminal or failed task must not produce a settlement amount, otherwise
// the polling loop would charge for work that never delivered.
func TestAdjustBillingOnCompleteOnlySettlesSuccess(t *testing.T) {
	payload := []byte(`{"id":"v1_abc","status":"completed","usage":{"total_tokens":100,"total_input_tokens":40,"total_output_tokens":60}}`)
	task := &model.Task{TaskID: "v1_abc", Data: payload}

	for _, status := range []string{model.TaskStatusInProgress, model.TaskStatusFailure, model.TaskStatusQueued} {
		quota := (&TaskAdaptor{}).AdjustBillingOnComplete(task, &relaycommon.TaskInfo{Status: status})
		assert.Zero(t, quota, "status %s must not settle", status)
	}
}

// TestDoResponseAdoptsUpstreamInteractionID is a regression test for a real bug:
// the response body forwarded to the client carries the upstream interaction id,
// but RelayTaskSubmit pre-generates a task_xxxx public id that InitTask would
// otherwise store as Task.TaskID. The two diverging made the stored task
// unreachable — polling the id the client was handed returned 404.
// DoResponse must adopt the upstream id as the public task id so the stored row,
// the task log and the response body all agree.
func TestDoResponseAdoptsUpstreamInteractionID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	const upstreamID = "task_353512800800210944"
	body := `{"id":"` + upstreamID + `","object":"interaction","status":"in_progress"}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}

	info := &relaycommon.RelayInfo{
		TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_pregenerated_placeholder"},
	}

	taskID, taskData, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)

	require.Nil(t, taskErr)
	assert.Equal(t, upstreamID, taskID, "upstream id must be returned as the task id")
	assert.Equal(t, upstreamID, info.PublicTaskID,
		"public task id must be replaced so InitTask stores the id the client received")
	assert.JSONEq(t, body, string(taskData))
	assert.JSONEq(t, body, recorder.Body.String(), "client must receive the upstream body verbatim")
}

// TestDoResponseRejectsMissingInteractionID keeps a malformed upstream reply from
// creating a task row with an empty id, which the polling loop would immediately
// mark as failed.
func TestDoResponseRejectsMissingInteractionID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"object":"interaction","status":"in_progress"}`)),
		Header:     http.Header{},
	}
	info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_keep"}}

	_, _, taskErr := (&TaskAdaptor{}).DoResponse(c, resp, info)

	require.NotNil(t, taskErr)
	assert.Equal(t, "task_keep", info.PublicTaskID, "a rejected submit must not mutate the public id")
}
