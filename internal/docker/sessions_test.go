package docker

import (
	"reflect"
	"testing"
)

func TestParseTopOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   string
		expected []TopProcess
	}{
		{
			name:     "empty output",
			output:   "",
			expected: nil,
		},
		{
			name:     "header only",
			output:   "PID   PPID  USER     ARGS\n",
			expected: nil,
		},
		{
			name: "single process",
			output: `PID   PPID  USER     ARGS
100   1     root     /bin/bash /sbin/boot
`,
			expected: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
			},
		},
		{
			name: "multiple processes",
			output: `PID   PPID  USER       ARGS
2811456  2811430  root       /bin/bash /sbin/boot --sysctl kernel.unprivileged_userns_clone=1
2811520  2811456  root       /usr/bin/supervisord
2811600  2811520  discourse  unicorn master
2815711  2815690  discourse  bash -l
`,
			expected: []TopProcess{
				{PID: 2811456, PPID: 2811430, User: "root", Args: "/bin/bash /sbin/boot --sysctl kernel.unprivileged_userns_clone=1"},
				{PID: 2811520, PPID: 2811456, User: "root", Args: "/usr/bin/supervisord"},
				{PID: 2811600, PPID: 2811520, User: "discourse", Args: "unicorn master"},
				{PID: 2815711, PPID: 2815690, User: "discourse", Args: "bash -l"},
			},
		},
		{
			name: "malformed lines skipped",
			output: `PID   PPID  USER  ARGS
abc   1     root  something
100   xyz   root  something
100   1
100   1     root  valid line
`,
			expected: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "valid line"},
			},
		},
		{
			name: "args with many spaces",
			output: `PID   PPID  USER  ARGS
100   50    root  claude --dangerously-skip-permissions -p hello world
`,
			expected: []TopProcess{
				{PID: 100, PPID: 50, User: "root", Args: "claude --dangerously-skip-permissions -p hello world"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTopOutput(tt.output)
			if err != nil {
				t.Fatalf("ParseTopOutput() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("ParseTopOutput() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestFindExecSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		procs    []TopProcess
		initPID  int
		expected []ExecSession
	}{
		{
			name:     "empty process list",
			procs:    nil,
			initPID:  100,
			expected: nil,
		},
		{
			name: "only init process",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
			},
			initPID:  100,
			expected: nil,
		},
		{
			name: "init with internal children only",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
				{PID: 200, PPID: 100, User: "root", Args: "/usr/bin/supervisord"},
				{PID: 300, PPID: 200, User: "discourse", Args: "unicorn master"},
			},
			initPID:  100,
			expected: nil,
		},
		{
			name: "one exec session",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
				{PID: 200, PPID: 100, User: "root", Args: "/usr/bin/supervisord"},
				{PID: 500, PPID: 99, User: "discourse", Args: "bash -l"},
			},
			initPID: 100,
			expected: []ExecSession{
				{PID: 500, User: "discourse", Command: "bash -l"},
			},
		},
		{
			name: "multiple exec sessions",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
				{PID: 200, PPID: 100, User: "root", Args: "/usr/bin/supervisord"},
				{PID: 500, PPID: 99, User: "discourse", Args: "bash -l"},
				{PID: 600, PPID: 88, User: "discourse", Args: "claude --dangerously-skip-permissions"},
			},
			initPID: 100,
			expected: []ExecSession{
				{PID: 500, User: "discourse", Command: "bash -l"},
				{PID: 600, User: "discourse", Command: "claude --dangerously-skip-permissions"},
			},
		},
		{
			name: "exec session children not double-counted",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
				{PID: 500, PPID: 99, User: "discourse", Args: "bash -l"},
				{PID: 501, PPID: 500, User: "discourse", Args: "vim file.txt"},
			},
			initPID: 100,
			expected: []ExecSession{
				{PID: 500, User: "discourse", Command: "bash -l"},
			},
		},
		{
			name: "init PID 0 does not match real processes",
			procs: []TopProcess{
				{PID: 100, PPID: 1, User: "root", Args: "/bin/bash /sbin/boot"},
				{PID: 500, PPID: 99, User: "discourse", Args: "bash -l"},
			},
			initPID: 0,
			expected: []ExecSession{
				{PID: 100, User: "root", Command: "/bin/bash /sbin/boot"},
				{PID: 500, User: "discourse", Command: "bash -l"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FindExecSessions(tt.procs, tt.initPID)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("FindExecSessions() = %+v, want %+v", got, tt.expected)
			}
		})
	}
}
