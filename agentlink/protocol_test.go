package agentlink

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in testdata (err=%v)", err)
	}
	sort.Strings(files)
	return files
}

func asMap(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A1: every fixture decodes into its Go struct and re-encodes to semantically
// equal JSON, and a fixture whose type has no Go struct fails the test.
func TestFixturesRoundTrip(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range fixtureFiles(t) {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			env, err := DecodeEnvelope(raw)
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			if !KnownType(env.Type) {
				t.Fatalf("fixture type %q has no Go struct", env.Type)
			}
			seen[env.Type] = true

			payload, err := DecodePayload(env)
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			reencoded := Envelope{V: env.V, ID: env.ID, Type: env.Type, TS: env.TS, Payload: mustJSON(t, payload)}

			want := asMap(t, raw)
			got := asMap(t, mustJSON(t, reencoded))
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip lost or changed something\nwant %s\n got %s", raw, mustJSON(t, reencoded))
			}
		})
	}

	// Every message type of the protocol must be covered by a fixture too.
	for typ := range payloadTypes {
		if !seen[typ] {
			t.Errorf("no fixture covers type %q", typ)
		}
	}
}

func TestEmptyListsSurviveRoundTrip(t *testing.T) {
	// "media": [] and "buttons": [] mean something; neither may become null or vanish.
	in := `{"conversation":"wa:1@s.whatsapp.net","card_id":"c","text":"t","buttons":[],"expires_at":"2026-10-01T09:40:12.345Z"}`
	var e EditCard
	if err := json.Unmarshal([]byte(in), &e); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asMap(t, []byte(in)), asMap(t, mustJSON(t, e))) {
		t.Errorf("edit_card buttons: [] changed: %s", mustJSON(t, e))
	}

	out := mustJSON(t, CustomerMessage{Conversation: "wa:1@s.whatsapp.net", TopicID: "1", HubMsgID: "x", Media: nonNilMedia(nil)})
	if !bytes.Contains(out, []byte(`"media":[]`)) {
		t.Errorf("media must encode as [], got %s", out)
	}
}

func TestFixtureCopiesMatchTheAgentRepo(t *testing.T) {
	src := filepath.Join("..", "..", "cm_website_astro", "clarus-agent", "protocol", "fixtures")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Skip("the clarus-agent checkout is not next to this repo")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		want, _ := os.ReadFile(filepath.Join(src, e.Name()))
		got, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Errorf("testdata is missing %s", e.Name())
			continue
		}
		if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("testdata/%s differs from the agent repo's fixture", e.Name())
		}
	}
}

func TestDecodeEnvelopeRejectsBadFrames(t *testing.T) {
	for name, frame := range map[string]string{
		"not json":     `nope`,
		"wrong v":      `{"v":2,"id":"a","type":"ack","ts":"2026-10-01T09:30:12.345Z","payload":{}}`,
		"no id":        `{"v":1,"type":"ack","ts":"2026-10-01T09:30:12.345Z","payload":{}}`,
		"no type":      `{"v":1,"id":"a","ts":"2026-10-01T09:30:12.345Z","payload":{}}`,
		"bad ts":       `{"v":1,"id":"a","type":"ack","ts":"yesterday","payload":{}}`,
		"missing ts":   `{"v":1,"id":"a","type":"ack","payload":{}}`,
		"array":        `[]`,
		"string v":     `{"v":"1","id":"a","type":"ack","ts":"2026-10-01T09:30:12.345Z","payload":{}}`,
		"empty object": `{}`,
	} {
		if _, err := DecodeEnvelope([]byte(frame)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodePayloadValidation(t *testing.T) {
	env := func(typ, payload string) Envelope {
		return Envelope{V: 1, ID: "x", Type: typ, TS: "2026-10-01T09:30:12.345Z", Payload: json.RawMessage(payload)}
	}
	bad := map[string]Envelope{
		"send without expires_at": env("send", `{"conversation":"wa:1@s.whatsapp.net","kind":"note","media":[]}`),
		"send bad kind":           env("send", `{"conversation":"wa:1@s.whatsapp.net","kind":"shout","media":[],"expires_at":"2026-10-01T09:40:12.345Z"}`),
		"send bad conversation":   env("send", `{"conversation":"60123","kind":"note","media":[],"expires_at":"2026-10-01T09:40:12.345Z"}`),
		"send without media":      env("send", `{"conversation":"wa:1@s.whatsapp.net","kind":"note","expires_at":"2026-10-01T09:40:12.345Z"}`),
		"send bad media kind":     env("send", `{"conversation":"wa:1@s.whatsapp.net","kind":"reply","media":[{"kind":"gif","mime":"a/b","size":1}],"expires_at":"2026-10-01T09:40:12.345Z"}`),
		"long button id":          env("send", `{"conversation":"wa:1@s.whatsapp.net","kind":"card","media":[],"buttons":[[{"id":"123456789012345678901234567890123","label":"x"}]],"expires_at":"2026-10-01T09:40:12.345Z"}`),
		"ack without id":          env("ack", `{}`),
		"payload missing":         {V: 1, ID: "x", Type: "ack", TS: "2026-10-01T09:30:12.345Z"},
		"unknown type":            env("presence", `{}`),
	}
	for name, e := range bad {
		if _, err := DecodePayload(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
