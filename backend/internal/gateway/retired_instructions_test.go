package gateway

import (
	"net/http"
	"testing"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"github.com/tidwall/gjson"
)

func TestBuildWSRequestIgnoresRetiredForceInstructionsHeader(t *testing.T) {
	req := &sdk.ForwardRequest{
		Model:   "gpt-5.6-sol",
		Body:    []byte("{\"input\":\"hello\",\"instructions\":\"client instructions\"}"),
		Headers: http.Header{},
	}
	req.Headers.Set("X-Airgate-Force-Instructions", "old group override")
	body, err := (&OpenAIGateway{}).buildWSRequest(req, openAISessionResolution{})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "instructions").String(); got != "client instructions" {
		t.Fatalf("retired group override changed client instructions: %q", got)
	}
}
