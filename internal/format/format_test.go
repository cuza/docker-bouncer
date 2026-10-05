package format

import "testing"

func TestParseAndOrder(t *testing.T) {
	f, err := Parse("1.10")
	if err != nil || f != (Format{1, 10}) || f.String() != "1.10" {
		t.Fatalf("%v %v", f, err)
	}
	for _, bad := range []string{"", "1", "a.b", "1.-1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if !(Format{1, 9}).Less(Format{1, 10}) || !(Format{1, 10}).Less(Format{2, 0}) || (Format{1, 0}).Less(Format{1, 0}) {
		t.Fatal("order")
	}
}
