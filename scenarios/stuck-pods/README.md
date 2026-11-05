# Stuck pods

Pods of a Deployment never become ready: no node takes them, their image can't be pulled, a volume isn't there, an init container never finishes, their configuration is missing, or a namespace quota refuses them. Themis works out which, and hands the incident to a person with the cause and what to look at. It never changes the workload.

## Alerts

`KubePodNotReady`, `KubeContainerWaiting` and `KubeDeploymentRolloutStuck`, about a Deployment.

None of them is an alert the rollout-regression pack answers, and this pack's priority, 50, is below that pack's, 100: if a profile is changed so both answer an alert, rollout-regression takes it. A bad rollout raises `KubeDeploymentReplicasMismatch` too, so rollout-regression opens its own incident for it and may suggest a rollback there, while this pack's incident only explains.

## Evidence

| Template | Why |
|---|---|
| `pod_states` | Which pods aren't ready, of which revision, and what each container waits for |
| `target_events` | What the scheduler, the kubelet and the ReplicaSet said about them: why a pod can't be scheduled, pulled, mounted or created |
| `rollout_history` | Which revision is current, and whether the one before it still runs |
| `referenced_config` | Whether a ConfigMap the pods need is missing |
| `container_logs_tail` | Asked for when a container runs but isn't ready |

## Checks

Each looks at the pods that aren't ready, and is false when every pod is ready.

| Check | True when |
|---|---|
| `scheduler_rejected_pods` | The scheduler can't place one: too little CPU or memory, a taint, an affinity, or a volume no node satisfies |
| `image_pull_failing` | A container waits for its image |
| `volume_not_bound` | An event says a volume can't be bound, attached or mounted |
| `init_container_stuck` | An init container failed, or has run for over 5 minutes |
| `config_error_on_start` | A container can't be created for a missing ConfigMap, Secret or key, or a ConfigMap the pods need doesn't exist |
| `quota_refused_pods` | A namespace quota refused a pod its ReplicaSet tried to create |
| `stuck_pods_new_revision_only` | Every stuck pod is of the current revision, and the revision before it runs ready pods |

Some causes can't be read from Themis's evidence, and are left to the investigator: why the scheduler refused, which credentials a registry wants, why a claim isn't bound (the claim and its events aren't read), what an init container waits for, which key a Secret lacks (Secrets are never read), and which quota refused a pod beyond what the event says.

## Fix

None: this pack diagnoses and escalates, and its policy refuses every fix. When `stuck_pods_new_revision_only` is true, the new version may be broken and a rollback right; the investigator may say so, and the person it escalates to, or the rollout-regression incident on the same Deployment, decides.

## Cases

| Case | Root cause |
|---|---|
| `unschedulable-taint` | Cordoned nodes, and the rest tainted for other work |
| `image-pull-auth` | The registry refuses expired credentials |
| `volume-claim-pending` | The claim names a StorageClass that doesn't exist |
| `init-container-waits` | An init container waits for a lock a crashed job left |
| `config-key-missing` | A Secret lost a key the pods read |
| `quota-exceeded` | The quota has no room for the new revision's first pod |

In each, a rollback is wrong: the earlier revision fails the same way, or there is none.

## Look-alikes

| Case | What it is |
|---|---|
| `rollout-bad-image` | A new revision with a tag that doesn't exist, the one before it healthy: a bad rollout. A rollback is the rollout-regression pack's call; this pack names the cause and escalates |
| `slow-startup` | A new revision's pod that is slow to get ready but working, where a rollback would throw a good release away |
