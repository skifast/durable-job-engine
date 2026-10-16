package canonical

import "testing"

// Number samples from RFC 8785 Appendix B (the JSON text, then what the scheme must write).
func TestNumbers(t *testing.T) {
	cases := map[string]string{
		"0":                             "0",
		"-0":                            "0",
		"9007199254740992":              "9007199254740992",
		"-9007199254740991":             "-9007199254740991",
		"4.50":                          "4.5",
		"2e-3":                          "0.002",
		"0.000001":                      "0.000001",
		"0.0000001":                     "1e-7",
		"1e21":                          "1e+21",
		"1e30":                          "1e+30",
		"123456789012345678901":         "123456789012345680000",
		"333333333.33333329":            "333333333.3333333",
		"0.000000000000000000000000001": "1e-27",
		"5e-324":                        "5e-324",
		"1.7976931348623157e308":        "1.7976931348623157e+308",
		"1.5e300":                       "1.5e+300",
		"100":                           "100",
		"1E2":                           "100",
		"0.1":                           "0.1",
	}
	for in, want := range cases {
		got, err := Marshal([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q (err %v), want %q", in, got, err, want)
		}
	}
}

func TestNumberOutOfRange(t *testing.T) {
	if _, err := Marshal([]byte("1e999")); err == nil {
		t.Error("1e999 should be refused: it does not fit in a double")
	}
}

// RFC 8785 section 3.2.3: keys sort by UTF-16 code units.
func TestKeyOrderUTF16(t *testing.T) {
	in := `{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh",` +
		`"1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"\u00f6\":\"Latin Small Letter O With Diaeresis\"," +
		"\"\u20ac\":\"Euro Sign\",\"\U0001F600\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"
	got, err := Marshal([]byte(in))
	if err != nil || string(got) != want {
		t.Errorf("got  %q (err %v)\nwant %q", got, err, want)
	}
}

// The main example from RFC 8785 section 3.2.2.
func TestRFCExample(t *testing.T) {
	in := `{
	  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
	  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
	  "literals": [null, true, false]
	}`
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],` +
		"\"string\":\"\u20ac$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}"
	got, err := Marshal([]byte(in))
	if err != nil || string(got) != want {
		t.Errorf("got  %s (err %v)\nwant %s", got, err, want)
	}
}

// Same data, different key order and spacing: same bytes.
func TestOrderAndSpacingDoNotMatter(t *testing.T) {
	a, _ := Marshal([]byte(`{"b": [1, 2], "a": {"y": 1, "x": 2}}`))
	b, _ := Marshal([]byte(`{"a":{"x":2,"y":1},"b":[1,2]}`))
	if string(a) != string(b) || string(a) != `{"a":{"x":2,"y":1},"b":[1,2]}` {
		t.Errorf("got %s and %s", a, b)
	}
}

// Go's own encoder would write \u003c here; the scheme leaves these characters alone.
func TestNoHTMLEscaping(t *testing.T) {
	got, _ := Marshal([]byte(`"<a href=\"x\">&</a>"`))
	if string(got) != `"<a href=\"x\">&</a>"` {
		t.Errorf("got %s", got)
	}
}

func TestRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"not json":      `{a:1}`,
		"trailing data": `{} {}`,
		"empty":         ``,
		"invalid UTF-8": "\"\xff\"",
	} {
		if _, err := Marshal([]byte(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
