# Prototype outputs

What the TypeScript prototype wrote for the VM intents of `TestVMEditsMatchThePrototype`,
generated on 2026-10-27 from `packages/core` (before the prototype was removed) with:

```ts
// create: serializeDocument("x.yaml", orderBySchema(doc, vmSchema)) for the form values of the
// test, with owner "alice"
// resize: patchYaml(web01, { ...web01, cpu: 8, memory_gb: 32 })
// extend-disk: patchYaml(db01, db01 with disks.0.size_gb = 250)
```

`web01` and `db01` are `envs/prod/vms/*.yaml` of the example repository. These files are not
regenerated: they pin the prototype's behaviour that the Go engine reproduces byte for byte.
