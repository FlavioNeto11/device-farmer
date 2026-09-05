package fakeadb

import "testing"

// The shell half of the sync server exists to hold a client to its quoting.
// That only works if the fake's own parser is strict, so the parser is tested
// on its own: a fake that accepted a bare path would let a mis-quoted client
// pass, and the injection guard the client claims would be untested.
func TestUnquoteShellWordAcceptsOnlyOneSingleQuotedWord(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{`'/data/local/tmp'`, "/data/local/tmp", true},
		{`'it'\''s'`, "it's", true},
		{`'a'\''b'\''c'`, "a'b'c", true},
		{`'/data/x; rm -rf /'`, "/data/x; rm -rf /", true},
		{`'$HOME/` + "`x`" + `'`, "$HOME/`x`", true},

		// What a careless client sends, each refused for its own reason.
		{`/data/local/tmp`, "", false},   // bare: the shell would split it
		{`"/data/local/tmp"`, "", false}, // double quotes still expand $ and `
		{`'/data/local/tmp`, "", false},  // unterminated
		{`'a' 'b'`, "", false},           // two words
		{`'a';'b'`, "", false},           // a second command
		{`'a'b'`, "", false},             // a bare stretch between quotes
		{`''`, "", false},                // no word at all
		{``, "", false},
	} {
		got, ok := unquoteShellWord(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("unquoteShellWord(%s) = %q, %t; want %q, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
