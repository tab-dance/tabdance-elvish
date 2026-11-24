package format

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSource(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{name: "empty", src: "", want: ""},
		{name: "simple", src: "echo x\n", want: "echo x\n"},
		{name: "extra spaces", src: "echo   x\n", want: "echo x\n"},
		{name: "trailing whitespace", src: "echo x  \n", want: "echo x\n"},
		{name: "preserve comment", src: "echo x # comment\n", want: "echo x # comment\n"},
		{name: "preserve indent", src: "if $true {\n  echo x\n}\n", want: "if $true {\n  echo x\n}\n"},
		{name: "normalize indent spacing inside line", src: "echo  x   y\n", want: "echo x y\n"},
		{name: "line continuation", src: "echo ^\n  x\n", want: "echo ^\n  x\n"},
		{name: "final newline added", src: "echo x", want: "echo x\n"},
		{name: "crlf preserved", src: "echo x\r\n", want: "echo x\r\n"},
		{name: "pipe spacing", src: "echo a|cat\n", want: "echo a | cat\n"},
		{name: "pipe spacing normalized", src: "echo a  |  cat\n", want: "echo a | cat\n"},
		{name: "redir spacing", src: "echo >out\n", want: "echo > out\n"},
		{name: "redir spacing normalized", src: "echo 2>  out\n", want: "echo 2> out\n"},
		{name: "append redir", src: "echo >>out\n", want: "echo >> out\n"},
		{name: "map pair compact", src: "put [&k=  v]\n", want: "put [&k=v]\n"},
		{name: "map pair preserves no-space", src: "put [&k=v &a=b]\n", want: "put [&k=v &a=b]\n"},
		{name: "redir fd out", src: "echo foo > /dev/null 2>&1\n", want: "echo foo > /dev/null 2>&1\n"},
		{name: "redir fd in", src: "echo >&2\n", want: "echo >&2\n"},
		{name: "preserve comment in string", src: "echo \"# not comment\"  # comment\n", want: "echo \"# not comment\" # comment\n"},
		{name: "lambda padding", src: "fn f {  echo a  }\n", want: "fn f { echo a }\n"},
		{name: "list padding", src: "put [ a  b ]\n", want: "put [ a b ]\n"},
		{name: "map padding before close", src: "put [&k=v ]\n", want: "put [&k=v ]\n"},
		{name: "brace no extra padding multiline", src: "if $true {\n  echo a\n}\n", want: "if $true {\n  echo a\n}\n"},
		{name: "lambda param concise", src: "fn quote {|string| }\n", want: "fn quote {|string| }\n"},
		{name: "lambda param normalize spaced", src: "fn quote { | string | }\n", want: "fn quote {|string| }\n"},
		{name: "lambda param normalize mixed", src: "{|x y| echo $x}\n", want: "{|x y| echo $x}\n"},
		{name: "lambda param with opt", src: "{|a &b=2| put $a}\n", want: "{|a &b=2| put $a}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Source(tt.src, Options{})
			if err != nil {
				t.Fatalf("Source() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Source() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSourceParseError(t *testing.T) {
	_, err := Source("if {", Options{})
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSourceIdempotent(t *testing.T) {
	srcs := []string{
		"echo  x\n",
		"if $true {\n  echo   x # c\n}\n",
		"var x = 1\nset x = 2\n",
		"fn f {|a &b=1| echo $a | cat >out 2>&1 }\n",
		"put [&k=  v  &x=y]\n",
	}
	for _, src := range srcs {
		got1, err := Source(src, Options{})
		if err != nil {
			t.Fatalf("first format: %v", err)
		}
		got2, err := Source(got1, Options{})
		if err != nil {
			t.Fatalf("second format: %v", err)
		}
		if got1 != got2 {
			t.Fatalf("not idempotent:\nfirst:  %q\nsecond: %q", got1, got2)
		}
	}
}

func TestSourceGolden(t *testing.T) {
	tests := []string{"basic", "containers", "trivia"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", name+".input.elv"))
			if err != nil {
				t.Fatalf("read input: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", name+".golden.elv"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			got, err := Source(string(input), Options{})
			if err != nil {
				t.Fatalf("Source: %v", err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch\n got: %q\nwant: %q", got, string(want))
			}
			got2, err := Source(got, Options{})
			if err != nil {
				t.Fatalf("Source second: %v", err)
			}
			if got2 != got {
				t.Fatalf("golden not idempotent:\nfirst: %q\nsecond: %q", got, got2)
			}
		})
	}
}
