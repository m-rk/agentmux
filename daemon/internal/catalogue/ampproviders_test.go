package catalogue

import (
	"errors"
	"testing"
)

func TestCheckMasked(t *testing.T) {
	ok := []string{
		"1. ChatGPT subscription (active)\n2. anthropic key sk-ant-****wxyz\n",
		"openai  sk-…a1b2  active",
		"router  https://example.com/v1  active",
		"",
	}
	for _, s := range ok {
		if err := CheckMasked(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{
		"anthropic key sk-ant-api03abcdefghij active",
		"token Zm9vYmFyMTIzNDU2Nzg5MGFiY2RlZjEyMzQ1 active",
	}
	for _, s := range bad {
		if err := CheckMasked(s); !errors.Is(err, ErrUnmasked) {
			t.Errorf("%q: err = %v", s, err)
		}
	}
}

func TestChatGPTSubscriptionRefusesUnmasked(t *testing.T) {
	if has, err := ChatGPTSubscription("ChatGPT subscription active"); err != nil || !has {
		t.Errorf("has=%v err=%v", has, err)
	}
	if has, err := ChatGPTSubscription("anthropic key sk-ant-api03abcdefghij ChatGPT"); has || err == nil {
		t.Errorf("read unmasked output: has=%v err=%v", has, err)
	}
	if has, _ := ChatGPTSubscription("anthropic only"); has {
		t.Error("false positive")
	}
}
