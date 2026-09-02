package controller

import (
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/gin-gonic/gin"
)

// RelayGeminiInteractionFetch serves GET /v1beta/interactions/{id} for tasks
// submitted with "background": true.
//
// The stored task id is the upstream interaction id, so a client can keep using
// the official polling path unchanged. While the task is not in a terminal
// state the upstream is queried live and the task row refreshed, so progress is
// current even between background polling sweeps.
func RelayGeminiInteractionFetch(c *gin.Context) {
	interactionID := c.Param("interaction_id")
	if interactionID == "" {
		respondInteractionError(c, http.StatusBadRequest, "invalid_argument", "interaction id is required")
		return
	}
	userID := c.GetInt("id")

	task, exist, err := model.GetByTaskId(userID, interactionID)
	if err != nil {
		respondInteractionError(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !exist || task == nil {
		respondInteractionError(c, http.StatusNotFound, "not_found",
			fmt.Sprintf("interaction %s not found", interactionID))
		return
	}
	if task.Platform != constant.TaskPlatformGeminiInteractions {
		respondInteractionError(c, http.StatusNotFound, "not_found",
			fmt.Sprintf("interaction %s not found", interactionID))
		return
	}

	// Always prefer the upstream payload, including for completed tasks: the
	// stored copy has its generated media base64 truncated so a 6MB video never
	// lands in the database, which makes it unusable as a client response.
	// Storage is only a fallback for when the upstream cannot be reached.
	if body := fetchInteractionFromUpstream(c, task); len(body) > 0 {
		c.Data(http.StatusOK, "application/json", body)
		return
	}

	writeStoredInteraction(c, task)
}

// fetchInteractionFromUpstream queries the upstream for the latest state and
// persists any status change. It returns the raw upstream body, or nil when the
// live query could not be completed so the caller can fall back to storage.
func fetchInteractionFromUpstream(c *gin.Context, task *model.Task) []byte {
	channelModel, err := model.GetChannelById(task.ChannelId, true)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("interaction %s: channel %d unavailable: %s",
			task.TaskID, task.ChannelId, err.Error()))
		return nil
	}

	baseURL := constant.ChannelBaseURLs[channelModel.Type]
	if channelModel.GetBaseURL() != "" {
		baseURL = channelModel.GetBaseURL()
	}
	key := channelModel.Key
	if task.PrivateData.Key != "" {
		key = task.PrivateData.Key
	}

	adaptor := relay.GetTaskAdaptor(constant.TaskPlatformGeminiInteractions)
	if adaptor == nil {
		return nil
	}

	resp, err := adaptor.FetchTask(baseURL, key, map[string]any{
		"task_id": task.GetUpstreamTaskID(),
		"action":  task.Action,
	}, channelModel.GetSetting().Proxy)
	if err != nil || resp == nil {
		logger.LogWarn(c, fmt.Sprintf("interaction %s: upstream fetch failed: %v", task.TaskID, err))
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || len(body) == 0 {
		return nil
	}

	taskInfo, err := adaptor.ParseTaskResult(body)
	if err != nil || taskInfo == nil {
		logger.LogWarn(c, fmt.Sprintf("interaction %s: parse upstream result failed: %v", task.TaskID, err))
		return body
	}

	// Terminal states are left entirely to the polling sweep, which owns
	// settlement and refunds. Writing any part of a terminal state here would
	// desynchronise status from progress, and the sweep selects on
	// `progress != '100%'` — a task carrying 100% with a non-terminal status is
	// invisible to both the sweep and the timeout sweeper, so it would hang in
	// "in progress" forever with its pre-charged quota never settled.
	if taskInfo.Status == model.TaskStatusSuccess || taskInfo.Status == model.TaskStatusFailure {
		return body
	}

	snap := task.Snapshot()
	if taskInfo.Status != "" {
		task.Status = model.TaskStatus(taskInfo.Status)
	}
	if taskInfo.Progress != "" {
		task.Progress = taskInfo.Progress
	}
	if !snap.Equal(task.Snapshot()) {
		if _, updateErr := task.UpdateWithStatus(snap.Status); updateErr != nil {
			logger.LogWarn(c, fmt.Sprintf("interaction %s: persist progress failed: %s", task.TaskID, updateErr.Error()))
		}
	}

	return body
}

// writeStoredInteraction reports the task state in the official shape. It never
// returns the stored payload for a completed task, because generated media in it
// was truncated before persistence; clients are told to retry instead of being
// handed a corrupt video.
func writeStoredInteraction(c *gin.Context, task *model.Task) {
	if task.Status == model.TaskStatusSuccess {
		respondInteractionError(c, http.StatusServiceUnavailable, "unavailable",
			"interaction completed but the upstream result is temporarily unreachable, please retry")
		return
	}
	if len(task.Data) > 0 && task.Status == model.TaskStatusFailure {
		c.Data(http.StatusOK, "application/json", task.Data)
		return
	}
	payload := map[string]any{
		"id":     task.TaskID,
		"object": "interaction",
		"status": interactionStatusFromTask(task.Status),
	}
	if task.FailReason != "" {
		payload["error"] = map[string]any{"message": task.FailReason}
	}
	body, err := common.Marshal(payload)
	if err != nil {
		respondInteractionError(c, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}

// interactionStatusFromTask maps our task status back to the upstream status
// vocabulary so clients only ever see the official values.
func interactionStatusFromTask(status model.TaskStatus) string {
	switch status {
	case model.TaskStatusSuccess:
		return "completed"
	case model.TaskStatusFailure:
		return "failed"
	default:
		return "in_progress"
	}
}

func respondInteractionError(c *gin.Context, statusCode int, status string, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"code":    statusCode,
			"message": message,
			"status":  status,
		},
	})
}
