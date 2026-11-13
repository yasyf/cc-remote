package awskey

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
)

type operation int

const (
	getParameter operation = iota + 1
	exportCredentials
	getToken
)

type Command struct {
	op      operation
	profile string
	region  string
	name    string
	decrypt bool
}

var valueFlags = []string{"--profile", "--region", "--name", "--query", "--output", "--format", "--cluster-name"}

func Parse(argv []string) (Command, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "aws" {
		return Command{}, false
	}
	var words []string
	values := map[string]string{}
	decrypt := false
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--with-decryption" && !decrypt:
			decrypt = true
		case slices.Contains(valueFlags, arg):
			if _, seen := values[arg]; seen || i+1 == len(argv) {
				return Command{}, false
			}
			values[arg] = argv[i+1]
			i++
		case strings.HasPrefix(arg, "-"):
			return Command{}, false
		default:
			words = append(words, arg)
		}
	}
	command := Command{profile: values["--profile"], region: values["--region"], name: values["--name"], decrypt: decrypt}
	delete(values, "--profile")
	delete(values, "--region")
	switch {
	case slices.Equal(words, []string{"ssm", "get-parameter"}) && command.name != "" && values["--query"] == "Parameter.Value" && values["--output"] == "text" && len(values) == 3:
		command.op = getParameter
	case slices.Equal(words, []string{"configure", "export-credentials"}) && values["--format"] == "process" && len(values) == 1 && !decrypt:
		command.op = exportCredentials
	case slices.Equal(words, []string{"eks", "get-token"}) && values["--cluster-name"] != "" && !decrypt && only(values, "--cluster-name", "--output") && cmp.Or(values["--output"], "json") == "json":
		command.op, command.name = getToken, values["--cluster-name"]
	default:
		return Command{}, false
	}
	return command, true
}

func only(values map[string]string, allowed ...string) bool {
	for flag := range values {
		if !slices.Contains(allowed, flag) {
			return false
		}
	}
	return true
}
