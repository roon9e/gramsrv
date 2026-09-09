package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLegacyComposeConfigMatrix(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	root := repositoryRoot(t)
	compose := filepath.Join(root, "deploy", "docker-compose.yml")
	example, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		env  string
		want []string
	}{
		{name: "no env", env: "", want: []string{"POSTGRES_PASSWORD: telesrv", "REDIS_PASSWORD: telesrv"}},
		{name: "example", env: string(example), want: []string{"POSTGRES_PASSWORD: telesrv", "REDIS_PASSWORD: telesrv"}},
		{name: "custom passwords", env: "TELESRV_POSTGRES_PASSWORD=pg-custom\nTELESRV_REDIS_PASSWORD=redis-custom\n", want: []string{"POSTGRES_PASSWORD: pg-custom", "REDIS_PASSWORD: redis-custom"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envFile := filepath.Join(t.TempDir(), "empty.env")
			if tc.env != "" {
				envFile = filepath.Join(t.TempDir(), ".env")
			}
			if err := os.WriteFile(envFile, []byte(tc.env), 0600); err != nil {
				t.Fatal(err)
			}
			output := runComposeConfig(t, root, compose, envFile)
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Fatalf("compose config missing %q in output", want)
				}
			}
		})
	}
}

func TestLegacyComposePostgresPasswordRotation(t *testing.T) {
	if os.Getenv("TELESRV_RUN_COMPOSE_INTEGRATION") != "1" {
		t.Skip("set TELESRV_RUN_COMPOSE_INTEGRATION=1 to run the PostgreSQL volume upgrade probe")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	name := "gramsrv-legacy-pg-rotation-" + strings.ReplaceAll(t.Name(), "/", "-")
	volume := name + "-data"
	t.Cleanup(func() {
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
		_, _ = exec.Command("docker", "volume", "rm", "-f", volume).CombinedOutput()
	})

	runDocker(t, "volume", "create", volume)
	runDocker(t, "run", "-d", "--name", name, "-e", "POSTGRES_USER=telesrv", "-e", "POSTGRES_PASSWORD=old-password", "-v", volume+":/var/lib/postgresql/data", "postgres:17-alpine")
	waitForPostgres(t, name, "old-password")
	runDocker(t, "exec", "-e", "PGPASSWORD=old-password", name, "psql", "-U", "telesrv", "-d", "postgres", "-c", "ALTER ROLE telesrv PASSWORD 'new-password'")
	runDocker(t, "rm", "-f", name)
	runDocker(t, "run", "-d", "--name", name, "-e", "POSTGRES_USER=telesrv", "-e", "POSTGRES_PASSWORD=new-password", "-v", volume+":/var/lib/postgresql/data", "postgres:17-alpine")
	waitForPostgres(t, name, "new-password")

	cmd := exec.Command("docker", "exec", "-e", "PGPASSWORD=old-password", name, "psql", "-U", "telesrv", "-d", "postgres", "-c", "SELECT 1")
	if err := cmd.Run(); err == nil {
		t.Fatal("old PostgreSQL password still works after volume upgrade")
	}
}

func waitForPostgres(t *testing.T, container, password string) {
	t.Helper()
	for attempt := 0; attempt < 60; attempt++ {
		cmd := exec.Command("docker", "exec", "-e", "PGPASSWORD="+password, container, "pg_isready", "-U", "telesrv", "-d", "postgres")
		if cmd.Run() == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("PostgreSQL did not become ready")
}

func runDocker(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func runComposeConfig(t *testing.T, root, compose, envFile string) string {
	t.Helper()
	cmd := exec.Command("docker", "compose", "--env-file", envFile, "-f", compose, "config")
	cmd.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME=gramsrv-legacy-test")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, output)
	}
	return string(output)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(filename), ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}
