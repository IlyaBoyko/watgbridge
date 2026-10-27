package lzcustomer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// The same message as Lazada's API sends it (everything a string) and as its
// push sends it (real numbers and booleans) must decode to the same struct.
func TestMessageDecodesStringsAndNumbers(t *testing.T) {
	api := `{"message_id":"m1","session_id":"s1","from_account_type":"2","to_account_type":"1",
		"from_account_id":"100","to_account_id":"200","template_id":"1","type":"1","content":"{\"txt\":\"hi\"}",
		"send_time":"1623399917435","status":"0","auto_reply":"false","site_id":"MY"}`
	push := `{"message_id":"m1","session_id":"s1","from_account_type":2,"to_account_type":1,
		"from_account_id":100,"to_account_id":200,"template_id":1,"type":1,"content":"{\"txt\":\"hi\"}",
		"send_time":1623399917435,"status":0,"auto_reply":false,"site_id":"MY"}`

	var a, p Message
	if err := json.Unmarshal([]byte(api), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(push), &p); err != nil {
		t.Fatal(err)
	}
	if a != p {
		t.Errorf("API and push forms differ:\n api  %+v\n push %+v", a, p)
	}
	if int64(a.FromAccountType) != AccountSeller || int64(a.SendTime) != 1623399917435 || bool(a.AutoReply) {
		t.Errorf("decoded %+v", a)
	}
	if string(a.Content) != `{"txt":"hi"}` {
		t.Errorf("content = %q, want the raw JSON string", a.Content)
	}
}

func TestFlexTypesEdgeCases(t *testing.T) {
	var s struct {
		Big   FlexString `json:"big"`
		Empty FlexInt    `json:"empty"`
		Null  FlexInt    `json:"null"`
		Float FlexInt    `json:"float"`
		Yes   FlexBool   `json:"yes"`
		One   FlexBool   `json:"one"`
		No    FlexBool   `json:"no"`
		Blank FlexBool   `json:"blank"`
		Obj   FlexString `json:"obj"`
	}
	in := `{"big":1234567890123456789,"empty":"","null":null,"float":1.5e12,"yes":"true","one":1,"no":"false","blank":"","obj":{"a":1}}`
	if err := json.Unmarshal([]byte(in), &s); err != nil {
		t.Fatal(err)
	}
	if s.Big != "1234567890123456789" {
		t.Errorf("big id lost precision: %q", s.Big)
	}
	if s.Empty != 0 || s.Null != 0 || s.Float != 1500000000000 {
		t.Errorf("ints: %d %d %d", s.Empty, s.Null, s.Float)
	}
	if !bool(s.Yes) || !bool(s.One) || bool(s.No) || bool(s.Blank) {
		t.Errorf("bools: %v %v %v %v", s.Yes, s.One, s.No, s.Blank)
	}
	if s.Obj != `{"a":1}` {
		t.Errorf("object kept as %q", s.Obj)
	}

	var bad struct {
		N FlexInt `json:"n"`
	}
	if err := json.Unmarshal([]byte(`{"n":"abc"}`), &bad); err == nil {
		t.Error("a non-numeric string decoded as a number")
	}
	var badBool struct {
		B FlexBool `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"b":"maybe"}`), &badBool); err == nil {
		t.Error("a non-boolean string decoded as a bool")
	}
}

func TestTokenResponseDecodesStringLifetimes(t *testing.T) {
	var r TokenResponse
	in := `{"access_token":"a","refresh_token":"r","expires_in":"604800","refresh_expires_in":2592000,
		"country":"my","account":"x@y","country_user_info":[{"country":"MY","user_id":"u","seller_id":"s","short_code":"c"}]}`
	if err := json.Unmarshal([]byte(in), &r); err != nil {
		t.Fatal(err)
	}
	if r.ExpiresIn != 604800 || r.RefreshExpiresIn != 2592000 || r.CountryUserInfo[0].SellerID != "s" {
		t.Errorf("decoded %+v", r)
	}
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// Lazada's own documented answer, not our fake's rendition of it, must decode.
func TestDecodesLazadasSampleMessageList(t *testing.T) {
	body, err := os.ReadFile("testdata/message_list_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.Client(), srv.URL, "k", "s", nil, staticToken("tok"))
	page, err := c.MessageList(context.Background(), "100094063_2_1011822749_1_103", 0, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bool(page.HasMore) || page.NextStartTime != 1623399917435 || page.LastMessageID != "24jFlAu0BtRbP47190" {
		t.Errorf("page header: %+v", page)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("%d messages", len(page.Messages))
	}
	m := page.Messages[0]
	if m.MessageID != "23hR5b20BtRbT00001" || m.FromAccountType != AccountSeller || m.ToAccountType != AccountBuyer ||
		m.TemplateID != 1 || m.Type != TypeUser || m.SendTime != 1623399917435 || m.Status != 0 ||
		m.AutoReply || m.SiteID != "SG" || m.FromAccountID != "100094063" || m.ToAccountID != "1011822749" {
		t.Errorf("message: %+v", m)
	}
	if m.ProcessMsg != "NOTE: The message has not been sent. ..." {
		t.Errorf("process_msg = %q", m.ProcessMsg)
	}
	var content struct {
		Txt string `json:"txt"`
	}
	if err := json.Unmarshal([]byte(m.Content), &content); err != nil || content.Txt != "THIS IS AUTO REPLY out of working hours" {
		t.Errorf("content %q does not parse as the template-1 JSON: %v", m.Content, err)
	}
}
