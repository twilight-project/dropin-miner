package main

import "bytes"

// utf8BOM is the byte-order mark a Windows shell can put in front of text it
// writes to a program's standard input.
var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// trimUTF8BOM drops ONE leading byte-order mark and nothing else.
//
// A tolerance, not a change to the machine protocol: the request is still
// exactly one JSON object with nothing after it, and a second mark, a mark
// anywhere but the front, or any other leading byte is still refused. What
// makes it necessary is that the mark is not always ours to prevent —
// Windows PowerShell 5.1 puts one in front of text piped from inside a
// command (measured, see the execution harness), and a participant's shell
// profile can add one to a hook payload on a host we do not control.
//
// It does not replace rendering a command whose bytes arrive intact. The
// same shell that adds the mark also replaces every UTF-16 unit outside
// ASCII with "?", and no amount of tolerance here can recover a query the
// shell has already changed; only the rendered form can keep that from
// happening.
func trimUTF8BOM(b []byte) []byte {
	if after, ok := bytes.CutPrefix(b, utf8BOM); ok {
		return after
	}
	return b
}
