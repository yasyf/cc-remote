package awskey

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want Command
		ok   bool
	}{
		{
			"a parameter",
			[]string{"aws", "ssm", "get-parameter", "--profile", "ops", "--region", "us-west-2", "--name", "/ci/key", "--with-decryption", "--query", "Parameter.Value", "--output", "text"},
			Command{op: getParameter, profile: "ops", region: "us-west-2", name: "/ci/key", decrypt: true},
			true,
		},
		{
			"a parameter by absolute path with global flags first",
			[]string{"/opt/homebrew/bin/aws", "--profile", "ops", "ssm", "get-parameter", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "text"},
			Command{op: getParameter, profile: "ops", name: "/ci/key"},
			true,
		},
		{
			"process credentials",
			[]string{"aws", "configure", "export-credentials", "--profile", "lane", "--format", "process"},
			Command{op: exportCredentials, profile: "lane"},
			true,
		},
		{
			"an EKS token",
			[]string{"aws", "--region", "us-west-2", "eks", "get-token", "--cluster-name", "frg-eks", "--output", "json"},
			Command{op: getToken, region: "us-west-2", name: "frg-eks"},
			true,
		},
		{"an EKS token without an output flag", []string{"aws", "eks", "get-token", "--cluster-name", "frg-eks"}, Command{op: getToken, name: "frg-eks"}, true},
		{"an EKS token as yaml", []string{"aws", "eks", "get-token", "--cluster-name", "frg-eks", "--output", "yaml"}, Command{}, false},
		{"an EKS token for a role", []string{"aws", "eks", "get-token", "--cluster-name", "frg-eks", "--role-arn", "arn:x"}, Command{}, false},
		{"an EKS token with no cluster", []string{"aws", "eks", "get-token", "--output", "json"}, Command{}, false},
		{"a parameter with a cluster name", []string{"aws", "ssm", "get-parameter", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "text", "--cluster-name", "x"}, Command{}, false},
		{"json output", []string{"aws", "ssm", "get-parameter", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "json"}, Command{}, false},
		{"no query", []string{"aws", "ssm", "get-parameter", "--name", "/ci/key", "--output", "text"}, Command{}, false},
		{"no name", []string{"aws", "ssm", "get-parameter", "--query", "Parameter.Value", "--output", "text"}, Command{}, false},
		{"an unknown flag", []string{"aws", "ssm", "get-parameter", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "text", "--debug"}, Command{}, false},
		{"a repeated flag", []string{"aws", "ssm", "get-parameter", "--name", "/a", "--name", "/b", "--query", "Parameter.Value", "--output", "text"}, Command{}, false},
		{"a flag with no value", []string{"aws", "ssm", "get-parameter", "--query", "Parameter.Value", "--output", "text", "--name"}, Command{}, false},
		{"env credentials", []string{"aws", "configure", "export-credentials", "--profile", "lane", "--format", "env"}, Command{}, false},
		{"credentials with a name", []string{"aws", "configure", "export-credentials", "--name", "x", "--format", "process"}, Command{}, false},
		{"another operation", []string{"aws", "ssm", "get-parameters", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "text"}, Command{}, false},
		{"another tool", []string{"awsx", "ssm", "get-parameter", "--name", "/ci/key", "--query", "Parameter.Value", "--output", "text"}, Command{}, false},
		{"a shell", []string{"sh", "-c", "aws ssm get-parameter --name /ci/key --query Parameter.Value --output text"}, Command{}, false},
		{"nothing", nil, Command{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Parse(tt.argv)
			if ok != tt.ok || got != tt.want {
				t.Errorf("Parse(%q) = %+v, %v; want %+v, %v", tt.argv, got, ok, tt.want, tt.ok)
			}
		})
	}
}
