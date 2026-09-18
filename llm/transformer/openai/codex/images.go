package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
)

// The official Codex Images API accepts generation JSON directly, but edits
// require images[].image_url rather than public API multipart image uploads.
// See openai/codex codex-rs/codex-api/src/images.rs.
func (t *OutboundTransformer) transformNativeImageRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	// The Codex endpoint silently ignores mask, so reject it rather than
	// return an edit that can alter pixels outside the requested area.
	if req.Image != nil && len(req.Image.Mask) > 0 {
		return nil, fmt.Errorf("%w: masked image edits are not supported by the Codex Images endpoint", transformer.ErrInvalidRequest)
	}
	if req.APIFormat != llm.APIFormatOpenAIImageEdit && (req.Image == nil || len(req.Image.Images) == 0) {
		return t.imagesOutbound.TransformRequest(ctx, req)
	}
	if req.Image == nil || len(req.Image.Images) == 0 {
		return nil, fmt.Errorf("%w: at least one image is required for edits", transformer.ErrInvalidRequest)
	}
	copyReq := *req
	copyImage := *req.Image
	copyImage.Images = nil
	copyReq.Image = &copyImage
	copyReq.APIFormat = llm.APIFormatOpenAIImageGeneration
	result, err := t.imagesOutbound.TransformRequest(ctx, &copyReq)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		return nil, err
	}
	images := make([]map[string]string, len(req.Image.Images))
	for i, data := range req.Image.Images {
		images[i] = map[string]string{"image_url": imageDataURL(data)}
	}
	body["images"] = images
	if req.Image.InputFidelity != "" {
		body["input_fidelity"] = req.Image.InputFidelity
	}
	result.Body, err = json.Marshal(body)
	if err != nil {
		return nil, err
	}
	result.URL = strings.TrimSuffix(result.URL, "/images/generations") + "/images/edits"
	result.APIFormat = llm.APIFormatOpenAIImageEdit.String()
	return result, nil
}

func imageDataURL(data []byte) string {
	return "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
}
