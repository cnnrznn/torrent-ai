package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// knownEnv returns every TORRENT_AI_* variable serve understands.
func knownEnv(t *testing.T) map[string]bool {
	t.Helper()
	cfg := defaultConfig()
	r := newRegistry("torrentai serve", "")
	registerFlags(r, &cfg)
	serveCommand.register(r, &cfg)
	known := map[string]bool{}
	for name := range r.info {
		known[envName(name)] = true
	}
	return known
}

// TestDeployManifests checks the Dockerfile and Kubernetes manifests
// against the code, so a renamed flag or port can't silently break a
// deployment.
func TestDeployManifests(t *testing.T) {
	known := knownEnv(t)
	root := filepath.Join("..", "..")

	t.Run("Dockerfile env vars are real flags", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
		require.NoError(t, err)
		vars := regexp.MustCompile(`TORRENT_AI_[A-Z0-9_]+`).FindAllString(string(b), -1)
		require.NotEmpty(t, vars)
		for _, v := range vars {
			require.True(t, known[v], "Dockerfile uses %s, which is no flag", v)
		}
	})

	type container struct {
		Env []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
		Ports []struct {
			Name          string `yaml:"name"`
			ContainerPort int    `yaml:"containerPort"`
			Protocol      string `yaml:"protocol"`
		} `yaml:"ports"`
		ReadinessProbe struct {
			HTTPGet struct {
				Path string `yaml:"path"`
				Port string `yaml:"port"`
			} `yaml:"httpGet"`
		} `yaml:"readinessProbe"`
		LivenessProbe struct {
			HTTPGet struct {
				Path string `yaml:"path"`
				Port string `yaml:"port"`
			} `yaml:"httpGet"`
		} `yaml:"livenessProbe"`
	}
	type doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []container `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
			Ports []struct {
				Port int `yaml:"port"`
			} `yaml:"ports"`
		} `yaml:"spec"`
	}

	b, err := os.ReadFile(filepath.Join(root, "deploy", "k8s", "torrentai.yaml"))
	require.NoError(t, err)
	var docs []doc
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var d doc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		docs = append(docs, d)
	}

	var c *container
	for i := range docs {
		if docs[i].Kind == "StatefulSet" {
			c = &docs[i].Spec.Template.Spec.Containers[0]
		}
	}
	require.NotNil(t, c, "a StatefulSet")

	t.Run("env vars are real flags", func(t *testing.T) {
		require.NotEmpty(t, c.Env)
		for _, e := range c.Env {
			require.True(t, known[e.Name], "manifest sets %s, which is no flag", e.Name)
		}
		// Commented-out examples should be real too.
		for _, v := range regexp.MustCompile(`TORRENT_AI_[A-Z0-9_]+`).FindAllString(string(b), -1) {
			require.True(t, known[v], "manifest mentions %s, which is no flag", v)
		}
	})

	t.Run("ports agree with the flags and probes", func(t *testing.T) {
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		ports := map[string]int{}
		for _, p := range c.Ports {
			ports[p.Name] = p.ContainerPort
		}
		require.True(t, strings.HasSuffix(env["TORRENT_AI_API_ADDR"], ":8737"))
		require.Equal(t, 8737, ports["api"])
		require.Equal(t, "6881", env["TORRENT_AI_PORT"])
		require.Equal(t, 6881, ports["peers-tcp"])
		require.Equal(t, 6881, ports["peers-udp"])

		require.Equal(t, "/readyz", c.ReadinessProbe.HTTPGet.Path)
		require.Equal(t, "api", c.ReadinessProbe.HTTPGet.Port)
		require.Equal(t, "/healthz", c.LivenessProbe.HTTPGet.Path)

		// The peers Service must expose the same port the daemon
		// announces.
		for _, d := range docs {
			if d.Kind == "Service" {
				for _, p := range d.Spec.Ports {
					require.Contains(t, []int{6881, 8737}, p.Port)
				}
			}
		}
	})
}
