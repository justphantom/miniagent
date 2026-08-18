package responses

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestStreamClient_DoStreamResponsesBody(t *testing.T) {
	var streamSet bool
	client := &StreamClient{
		APIKey:   "sk",
		Endpoint: "https://api.test/v1/responses",
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			streamSet = strings.Contains(string(body), `"stream":true`)
			sse := strings.NewReader(strings.Join([]string{
				`data: {"type":"response.output_text.delta","delta":"he"}`,
				`data: {"type":"response.completed","response":` + responseBody() + `}`,
				"",
			}, "\n\n"))
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(sse), Header: http.Header{}, Request: req}, nil
		})},
	}
	var deltas []miniagent.Delta
	resp, err := client.DoStream(context.Background(), miniagent.Request{Model: "m", Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}}, func(d miniagent.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !streamSet || resp.Text != "answer" || len(deltas) != 1 {
		t.Errorf("streamSet=%v resp=%+v deltas=%d", streamSet, resp, len(deltas))
	}
}
