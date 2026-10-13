package license

import (
	"strings"
	"testing"
)

func TestParseStrictJSONAccepts(t *testing.T) {
	obj, err := parseStrictJSON([]byte(` {"a":1,"b":-0,"c":"x\u00e9\ud83d\ude00\n","d":[true,false,null],"e":{},"f":[],"g":9223372036854775807} `))
	if err != nil {
		t.Fatal(err)
	}
	if obj["a"] != int64(1) || obj["b"] != int64(0) || obj["c"] != "xé😀\n" || obj["g"] != int64(9223372036854775807) {
		t.Fatalf("unexpected values %#v", obj)
	}
	if d := obj["d"].([]interface{}); d[0] != true || d[1] != false || d[2] != nil {
		t.Fatalf("unexpected array %#v", d)
	}
}

func TestParseStrictJSONRejects(t *testing.T) {
	nested := func(levels int) string {
		return strings.Repeat(`{"a":`, levels) + "1" + strings.Repeat("}", levels)
	}
	cases := map[string]string{
		"not object":        `[1]`,
		"duplicate key":     `{"a":1,"a":1}`,
		"fraction":          `{"a":1.0}`,
		"exponent":          `{"a":1e3}`,
		"leading zero":      `{"a":01}`,
		"plus sign":         `{"a":+1}`,
		"NaN":               `{"a":NaN}`,
		"Infinity":          `{"a":Infinity}`,
		"21 chars":          `{"a":-12345678901234567890}`,
		"overflow":          `{"a":99999999999999999999}`,
		"lone high":         `{"a":"\ud800"}`,
		"lone low":          `{"a":"\udc00"}`,
		"reversed pair":     `{"a":"\ude00\ud83d"}`,
		"lone in key":       `{"\ud800":1}`,
		"raw control":       "{\"a\":\"x\x01\"}",
		"bad escape":        `{"a":"\x"}`,
		"invalid utf8":      "{\"a\":\"\xff\"}",
		"bom":               "\xef\xbb\xbf{}",
		"trailing data":     `{} {}`,
		"trailing comma":    `{"a":1,}`,
		"unterminated":      `{"a":"x`,
		"single quotes":     `{'a':1}`,
		"long key":          `{"` + strings.Repeat("k", 129) + `":1}`,
		"long string":       `{"a":"` + strings.Repeat("s", 4097) + `"}`,
		"long string utf16": `{"a":"` + strings.Repeat("😀", 2049) + `"}`,
		"depth 9 scalar":    nested(8),
		"empty":             ``,
	}
	for name, input := range cases {
		if _, err := parseStrictJSON([]byte(input)); err == nil {
			t.Errorf("%s: accepted %q", name, input)
		}
	}
}

func TestParseStrictJSONLimitsAreInclusive(t *testing.T) {
	ok := []string{
		`{"` + strings.Repeat("k", 128) + `":1}`,
		`{"a":"` + strings.Repeat("s", 4096) + `"}`,
		`{"a":"` + strings.Repeat("😀", 2048) + `"}`,
		`{"a":-1234567890123456789}`,
		strings.Repeat(`{"a":`, 7) + "1" + strings.Repeat("}", 7),
		strings.Repeat(`{"a":`, 7) + "{}" + strings.Repeat("}", 7),
	}
	for _, input := range ok {
		if _, err := parseStrictJSON([]byte(input)); err != nil {
			t.Errorf("rejected %.60q: %v", input, err)
		}
	}
}

func TestValidText(t *testing.T) {
	cases := []struct {
		s           string
		allowSpaces bool
		ok          bool
	}{
		{"oss-001", false, true},
		{"Customer A", true, true},
		{"Customer A", false, false},
		{" A", true, false},
		{"A ", true, false},
		{"客户 甲", true, true},
		{"😀", false, true},
		{"", true, false},
		{"a\tb", true, false},         // TAB is also a forbidden control character
		{"a\u00a0b", true, true},      // NBSP is whitespace, allowed inside
		{"a\u200bb", true, false},     // zero width space is forbidden
		{"a\ufeffb", true, false},     // BOM
		{"a\ufdd0b", true, false},     // noncharacter
		{"a\U0001fffeb", true, false}, // plane 1 noncharacter
		{"a\ue000b", true, false},     // private use
		{"a\u00adb", true, false},     // soft hyphen
		{strings.Repeat("a", 256), false, true},
		{strings.Repeat("a", 257), false, false},
		{strings.Repeat("😀", 128), false, true},
		{strings.Repeat("😀", 129), false, false},
	}
	for _, c := range cases {
		if got := validText(c.s, 256, c.allowSpaces); got != c.ok {
			t.Errorf("validText(%q, %v) = %v, want %v", c.s, c.allowSpaces, got, c.ok)
		}
	}
}
