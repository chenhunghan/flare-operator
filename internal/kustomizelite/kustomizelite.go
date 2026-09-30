// Package kustomizelite renders the small subset of kustomize that the examples use, so tests
// can apply `kubectl apply -k examples/...` content without the kustomize module (a large
// dependency) or a kubectl binary. It refuses every kustomization field it does not implement,
// so a kustomization that needs more fails loudly instead of rendering differently from
// kustomize. examples/examples_test.go compares its output with `kubectl kustomize` when kubectl
// is installed.
//
// Supported, with kustomize's semantics:
//   - namespace: set on every namespaced object; a Namespace object is renamed to it
//   - resources: YAML files, and directories holding a kustomization.yaml (rendered first)
//   - configMapGenerator: name, namespace, files ("path" or "key=path") and options (labels,
//     annotations, disableNameSuffixHash); UTF-8 files go to data, other files to binaryData
//   - generatorOptions: labels, annotations, disableNameSuffixHash
//
// Generated names must not be hashed (disableNameSuffixHash: true), and files must lie inside
// the kustomization's directory (kustomize's default load restriction).
package kustomizelite

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Kustomization is the supported part of kustomization.yaml.
type Kustomization struct {
	APIVersion         string            `json:"apiVersion,omitempty"`
	Kind               string            `json:"kind,omitempty"`
	Namespace          string            `json:"namespace,omitempty"`
	Resources          []string          `json:"resources,omitempty"`
	GeneratorOptions   *GeneratorOptions `json:"generatorOptions,omitempty"`
	ConfigMapGenerator []ConfigMapArgs   `json:"configMapGenerator,omitempty"`
}

// GeneratorOptions are generatorOptions, or a generator's options.
type GeneratorOptions struct {
	Labels                map[string]string `json:"labels,omitempty"`
	Annotations           map[string]string `json:"annotations,omitempty"`
	DisableNameSuffixHash *bool             `json:"disableNameSuffixHash,omitempty"`
}

// ConfigMapArgs is one configMapGenerator entry.
type ConfigMapArgs struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Files     []string          `json:"files,omitempty"`
	Options   *GeneratorOptions `json:"options,omitempty"`
}

// clusterScoped are the kinds kustomize's namespace transformer leaves without a namespace
// (the built-in cluster-scoped kinds the examples could plausibly contain).
var clusterScoped = map[string]bool{
	"Namespace": true, "CustomResourceDefinition": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"PersistentVolume": true, "StorageClass": true, "PriorityClass": true, "Node": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
}

// Build renders the kustomization in dir.
func Build(dir string) ([]*unstructured.Unstructured, error) {
	return build(dir, 0)
}

func build(dir string, depth int) ([]*unstructured.Unstructured, error) {
	if depth > 10 {
		return nil, fmt.Errorf("%s: kustomizations nested too deeply", dir)
	}
	k, err := load(dir)
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, r := range k.Resources {
		p, err := inside(dir, r)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("%s: resource %s: %w", dir, r, err)
		}
		var objs []*unstructured.Unstructured
		if fi.IsDir() {
			objs, err = build(p, depth+1)
		} else {
			objs, err = ReadManifests(p)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	for _, g := range k.ConfigMapGenerator {
		cm, err := generateConfigMap(dir, k.GeneratorOptions, g)
		if err != nil {
			return nil, err
		}
		out = append(out, cm)
	}
	if k.Namespace != "" {
		for _, o := range out {
			switch {
			case o.GetKind() == "Namespace" && o.GetAPIVersion() == "v1":
				o.SetName(k.Namespace)
			case !clusterScoped[o.GetKind()]:
				o.SetNamespace(k.Namespace)
			}
		}
	}
	seen := map[string]bool{}
	for _, o := range out {
		id := o.GroupVersionKind().GroupKind().String() + " " + o.GetNamespace() + "/" + o.GetName()
		if seen[id] {
			return nil, fmt.Errorf("%s: %s is defined twice", dir, id)
		}
		seen[id] = true
	}
	return out, nil
}

func load(dir string) (*Kustomization, error) {
	p := filepath.Join(dir, "kustomization.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var k Kustomization
	if err := yaml.UnmarshalStrict(b, &k); err != nil {
		return nil, fmt.Errorf("%s: %w (kustomizelite supports namespace, resources, configMapGenerator and generatorOptions only)", p, err)
	}
	if k.APIVersion != "" && k.APIVersion != "kustomize.config.k8s.io/v1beta1" {
		return nil, fmt.Errorf("%s: apiVersion %q", p, k.APIVersion)
	}
	if k.Kind != "" && k.Kind != "Kustomization" {
		return nil, fmt.Errorf("%s: kind %q", p, k.Kind)
	}
	return &k, nil
}

// inside resolves rel against dir and refuses paths that leave dir.
func inside(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s: absolute path %s", dir, rel)
	}
	p := filepath.Join(dir, rel)
	r, err := filepath.Rel(dir, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: %s is outside the kustomization's directory", dir, rel)
	}
	return p, nil
}

func generateConfigMap(dir string, global *GeneratorOptions, g ConfigMapArgs) (*unstructured.Unstructured, error) {
	if g.Name == "" {
		return nil, fmt.Errorf("%s: a configMapGenerator entry has no name", dir)
	}
	labels, annotations := map[string]any{}, map[string]any{}
	disable := false
	for _, o := range []*GeneratorOptions{global, g.Options} {
		if o == nil {
			continue
		}
		for k, v := range o.Labels {
			labels[k] = v
		}
		for k, v := range o.Annotations {
			annotations[k] = v
		}
		if o.DisableNameSuffixHash != nil {
			disable = *o.DisableNameSuffixHash
		}
	}
	if !disable {
		return nil, fmt.Errorf("%s: ConfigMap %s: name suffix hashes are not implemented (set disableNameSuffixHash: true)", dir, g.Name)
	}
	data, binary := map[string]any{}, map[string]any{}
	add := func(key string, v []byte) error {
		if _, dup := data[key]; dup {
			return fmt.Errorf("%s: ConfigMap %s: key %s is set twice", dir, g.Name, key)
		}
		if _, dup := binary[key]; dup {
			return fmt.Errorf("%s: ConfigMap %s: key %s is set twice", dir, g.Name, key)
		}
		if utf8.Valid(v) {
			data[key] = string(v)
		} else {
			binary[key] = base64.StdEncoding.EncodeToString(v)
		}
		return nil
	}
	for _, f := range g.Files {
		key, rel, ok := strings.Cut(f, "=")
		if !ok {
			key, rel = filepath.Base(f), f
		}
		p, err := inside(dir, rel)
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: ConfigMap %s: %w", dir, g.Name, err)
		}
		if err := add(key, b); err != nil {
			return nil, err
		}
	}
	meta := map[string]any{"name": g.Name}
	if g.Namespace != "" {
		meta["namespace"] = g.Namespace
	}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	obj := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta}
	if len(data) > 0 {
		obj["data"] = data
	}
	if len(binary) > 0 {
		obj["binaryData"] = binary
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// ReadManifests returns the non-empty YAML documents of file, decoded strictly (a duplicate
// key is an error).
func ReadManifests(file string) ([]*unstructured.Unstructured, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	for i := 0; ; i++ {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", file, i, err)
		}
		var m map[string]any
		if err := yaml.UnmarshalStrict(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", file, i, err)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
}
