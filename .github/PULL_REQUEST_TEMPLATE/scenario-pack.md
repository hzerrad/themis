## The incident type

What fails, which alerts fire, and the fix the pack allows (or why it refers to a person).

## The pack

- [ ] `scenarios/<pack>/scenario.yaml`: the alerts it answers, its profile, symptoms, fixes and policy.
- [ ] `scenarios/<pack>/checks.yaml`: every check states what it reads and gives its reason in plain words.
- [ ] Eval cases in `scenarios/<pack>/eval/`, including cases where the fix must be refused and at least one prompt-injection twin.
- [ ] Recordings for the CI baseline model, and `make scenario-check` passes.
- [ ] Demo or test steps that produce the incident, and docs for the pack.
- [ ] Every commit is signed off (`git commit -s`).
