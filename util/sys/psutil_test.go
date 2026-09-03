package sys

import "testing"

func TestHostProc(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		combineWith []string
		want        string
	}{
		{"缺省根", "", nil, "/proc"},
		{"缺省根拼一段", "", []string{"net/tcp"}, "/proc/net/tcp"},
		{"缺省根拼多段", "", []string{"net", "tcp6"}, "/proc/net/tcp6"},
		{"环境变量覆盖根", "/host/proc", nil, "/host/proc"},
		{"环境变量覆盖并拼接", "/host/proc", []string{"net/udp"}, "/host/proc/net/udp"},
		{"路径含冗余分隔符会被清理", "/host/proc/", []string{"/net/udp6"}, "/host/proc/net/udp6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOST_PROC", tt.env)
			if got := HostProc(tt.combineWith...); got != tt.want {
				t.Fatalf("HostProc(%q) = %q，期望 %q", tt.combineWith, got, tt.want)
			}
		})
	}
}
