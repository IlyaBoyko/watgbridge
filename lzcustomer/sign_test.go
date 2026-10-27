package lzcustomer

import "testing"

func TestSignVector(t *testing.T) {
	params := map[string]string{
		"app_key":      "123456",
		"timestamp":    "1517820392000",
		"access_token": "tok",
		"sign_method":  "sha256",
		"session_id":   "s1",
		"template_id":  "1",
		"txt":          "hello world",
		// Neither of these may change the result.
		"last_message_id": "",
		"sign":            "STALE",
	}
	const want = "48C89659C91D246AFCAF1636CCCBC54883C6C35FAFE9A7F9C0E46493D078D57F"
	if got := Sign("3412gyo124goi3124", "/im/message/send", params); got != want {
		t.Errorf("Sign = %s, want %s", got, want)
	}
}

func TestSignDependsOnPathAndSecret(t *testing.T) {
	p := map[string]string{"app_key": "k", "a": "1"}
	base := Sign("s", "/x", p)
	if Sign("s", "/y", p) == base {
		t.Error("path is not part of the signature")
	}
	if Sign("t", "/x", p) == base {
		t.Error("secret is not part of the signature")
	}
}

func TestVerifyPushVector(t *testing.T) {
	body := []byte(`{"seller_id":"1234567","message_type":2,"data":{"session_id":"s1"}}`)
	const header = "37fc22129875becebaf96b6b47e5b64aad0ef7f719ef0a09d79b143ccb75b41f"
	if !VerifyPush("123456", "3412gyo124goi3124", body, header) {
		t.Fatal("the documented push was rejected")
	}
	if !VerifyPush("123456", "3412gyo124goi3124", body, "37FC22129875BECEBAF96B6B47E5B64AAD0EF7F719EF0A09D79B143CCB75B41F") {
		t.Error("an uppercase header was rejected")
	}
	if VerifyPush("123456", "3412gyo124goi3124", append([]byte(" "), body...), header) {
		t.Error("a changed body was accepted")
	}
	if VerifyPush("123456", "other-secret", body, header) {
		t.Error("the wrong secret was accepted")
	}
	if VerifyPush("654321", "3412gyo124goi3124", body, header) {
		t.Error("the wrong app key was accepted")
	}
	if VerifyPush("123456", "3412gyo124goi3124", body, "not hex") || VerifyPush("123456", "3412gyo124goi3124", body, "") {
		t.Error("a malformed header was accepted")
	}
	if VerifyPush("123456", "", body, PushSignature("123456", "", body)) {
		t.Error("an empty secret accepted a push")
	}
}

func TestPushSignatureMatchesVector(t *testing.T) {
	body := []byte(`{"seller_id":"1234567","message_type":2,"data":{"session_id":"s1"}}`)
	const want = "37fc22129875becebaf96b6b47e5b64aad0ef7f719ef0a09d79b143ccb75b41f"
	if got := PushSignature("123456", "3412gyo124goi3124", body); got != want {
		t.Errorf("PushSignature = %s, want %s", got, want)
	}
}
