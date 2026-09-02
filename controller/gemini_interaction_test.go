package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
)

// TestInteractionStatusFromTaskMapsToUpstreamVocabulary keeps the polling
// response on the official status vocabulary regardless of our internal names.
func TestInteractionStatusFromTaskMapsToUpstreamVocabulary(t *testing.T) {
	cases := map[model.TaskStatus]string{
		model.TaskStatusSuccess:    "completed",
		model.TaskStatusFailure:    "failed",
		model.TaskStatusInProgress: "in_progress",
		model.TaskStatusQueued:     "in_progress",
		model.TaskStatusSubmitted:  "in_progress",
	}
	for status, want := range cases {
		assert.Equal(t, want, interactionStatusFromTask(status), "status %s", status)
	}
}
