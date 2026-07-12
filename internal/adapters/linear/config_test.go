package linear

import (
	"strings"
	"testing"
)

func TestDecodeConfigRejectsUnsafeOrIncompleteValuesWithoutLeakingReference(t *testing.T) {
	secret := "linear-secret-token"
	tests := []string{
		`{"api_url":"https://api.linear.app/graphql","credential_source_ref":"","authorization_scheme":"bearer","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`,
		`{"api_url":"https://api.linear.app/graphql","credential_source_ref":"` + secret + `","authorization_scheme":"bearer","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`,
		`{"api_url":"https://example.com/graphql","credential_source_ref":"secret://` + secret + `","authorization_scheme":"bearer","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`,
		`{"api_url":"https://api.linear.app/graphql","credential_source_ref":"secret://` + secret + `","authorization_scheme":"unsupported","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`,
		`{"api_url":"https://api.linear.app/graphql","credential_source_ref":"secret://` + secret + `","authorization_scheme":"bearer","team_key":"ifan","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`,
		`{"api_url":"https://api.linear.app/graphql","credential_source_ref":"secret://` + secret + `","authorization_scheme":"bearer","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2,"unknown":true}`,
	}
	for _, input := range tests {
		_, err := DecodeConfig(strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe config error: %v", err)
		}
	}
}

func TestDecodeConfigAcceptsFixtureEndpoint(t *testing.T) {
	config, err := DecodeConfig(strings.NewReader(`{"api_url":"http://127.0.0.1:8080/graphql","credential_source_ref":"secret://controller/linear-read","authorization_scheme":"bearer","team_key":"IFAN","http_timeout":"10s","max_response_bytes":4096,"label_page_size":10,"max_label_pages":2}`))
	if err != nil || config.TeamKey != "IFAN" {
		t.Fatalf("config=%+v error=%v", config, err)
	}
}
