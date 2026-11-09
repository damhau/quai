package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// lastState is a stack's Terraform state as the last apply kept it, found as the pipelines'
// restore-state.sh finds it: the "state" artifact of the newest completed run of the apply
// pipeline on main that has one; nil when none has.
func lastState(t *testing.T, repo *repositories.Repo, pipeline, env string) []byte {
	t.Helper()
	ctx := context.Background()
	p := repo.Connector.Pipelines()
	runs, err := p.Runs(ctx, pipeline, "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.State != "completed" {
			continue
		}
		b, err := p.Artifact(ctx, run.ID, "state", env+".tfstate")
		if err == nil {
			return b
		}
		if !errors.Is(err, connectors.ErrNotFound) {
			t.Fatal(err)
		}
	}
	return nil
}

// terraformPlan is the plan the plan pipeline's terraform plan writes for a stack at a commit,
// with the state the last apply kept (terraform show -json): what its quai report step would post.
// The stacks use the built-in terraform_data resource only, so no provider is downloaded.
func terraformPlan(t *testing.T, repo *repositories.Repo, applyPipeline, commit, stack string) json.RawMessage {
	t.Helper()
	ctx := context.Background()
	files, err := repo.Mirror.Files(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, root := range []string{"stacks", "modules", "vms"} {
		err := fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(files, p)
			if err != nil {
				return err
			}
			target := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	chdir := filepath.Join(dir, filepath.FromSlash(stack))
	if state := lastState(t, repo, applyPipeline, path.Base(stack)); state != nil {
		if err := os.WriteFile(filepath.Join(chdir, "terraform.tfstate"), state, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	terraform := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, "terraform", append([]string{"-chdir=" + chdir}, args...)...)
		cmd.Env = append(os.Environ(), "CHECKPOINT_DISABLE=1", "TF_IN_AUTOMATION=1", "TF_INPUT=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("terraform %s of %s at %s: %v\n%s", args[0], stack, commit, err, stderr.String())
		}
		return out
	}
	terraform("init", "-input=false", "-no-color")
	terraform("plan", "-input=false", "-no-color", "-out=tfplan")
	return terraform("show", "-json", "tfplan")
}

// TestPilotInfraRehearsal is the M2 exit rehearsed on the stand-in pilot-infra (P4-22), Quai
// connected to it as P2-13 onboarded it: the repository's own VM type, plan pipeline (its pull
// requests' build validation) and apply pipeline, its policies requiring quai/approval from Quai.
// It makes the ten changes of TestAuditSample, each through Quai's pull request, merged by Quai
// once approved or broken: the plans are the build validation runs Quai reads when it polls, with
// what their quai report step would post, Terraform's plan of each stack, which the test makes as
// the step would (the step cannot reach the test); the applies are the apply pipeline's runs,
// found by polling only. The auditor's ten bundles verify, as the audit log's export does, and the
// break-glass alert reaches a local Mailpit. Each created VM is deleted again, so pilot-infra's VM
// files end as they were.
//
// It changes a shared repository and runs its pipelines 19 times (12 minutes on 2026-11-09,
// longer when others' runs queue on the one agent), so it runs only when
// asked: QUAI_TEST_PILOT_INFRA names the stand-in's repository in the sandbox, with the live
// tests' QUAI_TEST_AZURE_DEVOPS_PAT_FILE and QUAI_TEST_AZURE_DEVOPS_URL, and Terraform on the
// PATH, the version the pipelines install (open question 124). QUAI_TEST_PILOT_INFRA_OUT names a
// directory for the key, the export and the bundles, to run quai audit verify on.
func TestPilotInfraRehearsal(t *testing.T) {
	name := os.Getenv("QUAI_TEST_PILOT_INFRA")
	if name == "" {
		t.Skip("QUAI_TEST_PILOT_INFRA is not set: the M2 rehearsal changes the stand-in pilot-infra, and runs only when asked")
	}
	sb := adotest.New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Fatal("the rehearsal plans as the plan pipeline does, with Terraform, which is not on the PATH")
	}
	out := os.Getenv("QUAI_TEST_PILOT_INFRA_OUT")
	plan, apply := name+"-plan", name+"-apply"
	mail := mailtest.Start(t, mailtest.Options{})
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  sync: { minInterval: 1ns }
  runner:
    reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN }
    pipelines:
      - { name: `+plan+`, kind: plan, stacks: "stacks/{env}" }
      - { name: `+apply+`, kind: apply, stacks: "stacks/{env}" }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxRunner{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	}, withMail(mail, "security@example.com"), withBreakGlass)
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	alice := h.as("alice")

	// A request left open by a failure is cancelled, which closes its pull request.
	requesters := map[string]string{}
	t.Cleanup(func() {
		for id, by := range requesters {
			if s := alice.read(id).State; s == "planned" || s == "pending_approval" || s == "approved" || s == "stale" {
				if status, raw := h.as(by).do(http.MethodPost, Prefix+"/requests/"+id+"/cancel", nil, nil); status != http.StatusOK {
					t.Errorf("request %s left %s: cancel = %d %s", id, s, status, raw)
				}
			}
		}
	})

	// pilot-infra's VM type: vCPUs and memory in MB; large from 8 vCPUs.
	sizes := map[int][2]int{1: {2, 4096}, 2: {4, 8192}, 3: {8, 16384}}
	vm := sampleType{
		create: func(env, name string, size int) map[string]any {
			return map[string]any{"repository": "infra-live", "type": "vm", "kind": "create", "params": map[string]string{"env": env, "name": name},
				"values": map[string]any{"description": "M2 rehearsal (P4-22)", "os_image": "ubuntu-24.04", "vcpu": sizes[size][0], "memory_mb": sizes[size][1],
					"subnet": env + "-app", "disks": []any{map[string]any{"size_gb": 40, "class": "standard"}}}}
		},
		resize: func(key string, size int) map[string]any {
			return map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": key, "action": "resize",
				"actionInputs": map[string]any{"vcpu": sizes[size][0], "memory_mb": sizes[size][1]}}
		},
	}
	runs := sampleRuns{
		plan: func(r RequestView) RequestView {
			requesters[r.ID] = r.Requester
			r = alice.awaitRuns(r.ID, "planned", func(v RequestView) bool {
				return len(v.Plans) == 2 && !slices.ContainsFunc(v.Plans, func(p RunView) bool { return p.Status != "succeeded" })
			})
			for _, p := range r.Plans {
				report := map[string]any{"kind": "plan", "stack": p.Stack, "pullRequest": p.PullRequest, "commit": p.Commit, "url": p.URL,
					"plan": map[string]any{"format": "terraform-json", "content": terraformPlan(t, repo, apply, r.Delivery.Head, p.Stack)}}
				if status, _, body := h.reportRun(runToken, runReport(report)); status != http.StatusAccepted {
					t.Fatalf("the quai report step of %s = %d %s", p.Stack, status, body)
				}
			}
			return alice.read(r.ID)
		},
		applied: func(r RequestView) RequestView {
			return alice.awaitRuns(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" || v.State == "failed" })
		},
	}
	tag := time.Now().UTC().Format("01021504")
	done := auditSample(t, h, mail, sampleChanges(vm, "m2-web-"+tag, "m2-db-"+tag, "m2-ci-"+tag, "m2-tmp-"+tag), runs, out)

	// Each plan is Terraform's: it changes the request's VM, as the request does.
	action := map[string]string{"create": "create", "action": "update", "delete": "delete"}
	for _, s := range done {
		r := s.request
		if s.bundle == nil {
			continue
		}
		p := slices.IndexFunc(s.bundle.Plans, func(p evidence.Run) bool { return p.Stack == r.Stack })
		if p < 0 {
			continue
		}
		var actions []string
		for _, res := range s.bundle.Plans[p].Resources {
			if res.Resource == r.Target {
				for _, c := range res.Changes {
					actions = append(actions, c.Action)
				}
			}
		}
		if !slices.Equal(actions, []string{action[r.Kind]}) {
			t.Errorf("request #%d (%s %s): the plan's changes to it %v, resources %+v", r.Number, r.Kind, r.Target, actions, s.bundle.Plans[p].Resources)
		}
		var plans, applies []string
		for _, run := range s.bundle.Plans {
			plans = append(plans, run.Stack+" "+run.URL)
		}
		for _, run := range s.bundle.Applies {
			applies = append(applies, run.Stack+" "+run.URL)
		}
		t.Logf("request #%d %s %s %s: pull request %s, merge commit %.12s, plans %s; applies %s; bundle: %d events, %d proven, checkpoints %v",
			r.Number, r.Kind, r.Target, r.State, r.Delivery.PullRequest, r.Delivery.MergeCommit, strings.Join(plans, ", "), strings.Join(applies, ", "),
			s.report.Events, s.report.Proven, s.report.Checkpoints)
	}
}
