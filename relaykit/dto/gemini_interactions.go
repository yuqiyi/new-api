package dto

import (
	"encoding/json"
	"net/http"
	"strings"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/gjson"
)

// GeminiInteractionsRequest keeps the complete request payload intact. The
// Interactions API accepts evolving multimodal input types, so modelling only
// its routing and billing fields prevents this gateway from dropping fields it
// does not yet know about.
type GeminiInteractionsRequest struct {
	Model           string
	Stream          *bool
	Background      *bool
	MaxOutputTokens *uint
	Body            map[string]json.RawMessage
}

func (r *GeminiInteractionsRequest) UnmarshalJSON(data []byte) error {
	var body map[string]json.RawMessage
	if err := kitutil.Unmarshal(data, &body); err != nil {
		return err
	}
	if body == nil {
		body = make(map[string]json.RawMessage)
	}

	r.Model = ""
	r.Stream = nil
	r.Background = nil
	r.MaxOutputTokens = nil
	r.Body = body
	if value, ok := body["model"]; ok {
		if err := kitutil.Unmarshal(value, &r.Model); err != nil {
			return err
		}
	}
	if value, ok := body["stream"]; ok {
		var stream bool
		if err := kitutil.Unmarshal(value, &stream); err != nil {
			return err
		}
		r.Stream = &stream
	}
	if value, ok := body["background"]; ok {
		var background bool
		if err := kitutil.Unmarshal(value, &background); err != nil {
			return err
		}
		r.Background = &background
	}
	for _, key := range []string{"max_output_tokens", "maxOutputTokens"} {
		value, ok := body[key]
		if !ok {
			continue
		}
		var maxTokens uint
		if err := kitutil.Unmarshal(value, &maxTokens); err != nil {
			return err
		}
		r.MaxOutputTokens = &maxTokens
		break
	}
	if r.MaxOutputTokens == nil {
		for _, configKey := range []string{"generation_config", "generationConfig"} {
			configValue, ok := body[configKey]
			if !ok {
				continue
			}
			var config map[string]json.RawMessage
			if err := kitutil.Unmarshal(configValue, &config); err != nil {
				return err
			}
			for _, key := range []string{"max_output_tokens", "maxOutputTokens"} {
				value, ok := config[key]
				if !ok {
					continue
				}
				var maxTokens uint
				if err := kitutil.Unmarshal(value, &maxTokens); err != nil {
					return err
				}
				r.MaxOutputTokens = &maxTokens
				break
			}
			if r.MaxOutputTokens != nil {
				break
			}
		}
	}
	return nil
}

func (r GeminiInteractionsRequest) MarshalJSON() ([]byte, error) {
	body := r.Body
	if body == nil {
		body = make(map[string]json.RawMessage)
	}
	model, err := kitutil.Marshal(r.Model)
	if err != nil {
		return nil, err
	}
	body["model"] = model
	return kitutil.Marshal(body)
}

func (r *GeminiInteractionsRequest) GetTokenCountMeta() *types.TokenCountMeta {
	meta := &types.TokenCountMeta{}
	if r == nil {
		return meta
	}
	if r.MaxOutputTokens != nil {
		meta.MaxTokens = int(*r.MaxOutputTokens)
	}
	input := r.Body["input"]
	if len(input) == 0 {
		return meta
	}
	var text strings.Builder
	collectGeminiInteractionText(gjson.ParseBytes(input), &text)
	meta.CombineText = text.String()
	return meta
}

func collectGeminiInteractionText(value gjson.Result, text *strings.Builder) {
	switch value.Type {
	case gjson.String:
		text.WriteString(value.String())
		text.WriteByte('\n')
	case gjson.JSON:
		if value.IsArray() {
			value.ForEach(func(_, item gjson.Result) bool {
				collectGeminiInteractionText(item, text)
				return true
			})
			return
		}
		value.ForEach(func(key, item gjson.Result) bool {
			if key.String() == "text" || key.String() == "input_text" {
				collectGeminiInteractionText(item, text)
			} else if item.Type == gjson.JSON {
				collectGeminiInteractionText(item, text)
			}
			return true
		})
	}
}

// IsBackground reports whether the client asked for background execution.
func (r *GeminiInteractionsRequest) IsBackground() bool {
	return r != nil && r.Background != nil && *r.Background
}

func (r *GeminiInteractionsRequest) IsStream(request *http.Request) bool {
	if r != nil && r.Stream != nil && *r.Stream {
		return true
	}
	return request != nil && request.URL.Query().Get("alt") == "sse"
}

func (r *GeminiInteractionsRequest) SetModelName(modelName string) {
	if r != nil {
		r.Model = modelName
	}
}
