# Benchmarks and LocalMaxxing

[← README](../README.md) · [Documentation index](../README.md#documentation)

`substrate bench` measures a served model and publishes the result to
[LocalMaxxing](https://localmaxxing.com). Measuring, verification evidence and
upload are done by [`lmx`](https://github.com/LottoLottoLotto/localmaxxing-cli)
(v0.1.56 or later, logged in with `lmx login`; `$SUBSTRATE_LMX` points at a
copy off `PATH`). substrate supplies what `lmx` would otherwise ask for.

```sh
substrate bench config flash-next-iq3s --hf-id ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF \
    --spec-method MTP --spec-num-tokens 3 --context-length 32768 --notes "..."   # once per model
substrate bench config flash-next-iq3s            # what a submission would say, and where each value comes from
substrate bench run flash-next-iq3s --validate    # measure; ask LocalMaxxing whether it would be verified
substrate bench run flash-next-iq3s --submit      # measure, validate, upload when verified
substrate bench run qwen3.8-27b-8u --concurrency 1,4,8,16
substrate bench list [--model M]                  # history: tok/s, prefill, TTFT, verdict, submission id
substrate bench show <id>                         # one run with its LocalMaxxing payload
substrate bench submit <id>...                    # upload recorded runs later
```

- **Metadata** (`bench:` on the model in `models.yaml`, set with
  `bench config`; `-` clears a value). Anything unset is detected:
  - the Hugging Face id from a repo-id source, the quantization from the
    registry;
  - the LocalMaxxing engine name from the engine (`llama.cpp`,
    `buun-llama-cpp` → llama.cpp, `vllm`/`tt-vllm` → vllm, `sglang`,
    `strata`, else `custom`); the engine version from the manifest's version
    command (a Strata server reports its own);
  - the engine commit and repository from the git checkout holding the
    engine binary, naming the remote whose branches contain that commit. A
    commit on no remote branch is reported as a problem: LocalMaxxing
    readers could not find the code;
  - the backend (`tt-metal`, `cuda`) and the hardware from the devices the
    instance holds: Tenstorrent board types from sysfs
    (`tt_card_type` p150a → "Tenstorrent P150", 32 GB), NVIDIA cards from
    `nvidia-smi`, the CPU, installed RAM and OS. `--hardware file.json`
    replaces the detected description.
- **Runs:** `bench run` starts the model if needed, then runs `lmx speed-test
  run --mode remote` once per `--concurrency` level on the canonical prompt
  (`--canonical-prompt`, default `reasoning-v1`; `--prompt-file` for your own,
  never verified). It measures the engine's own endpoint by default; `--via
  gateway` goes through `substrate serve`, where gateway policies, output caps
  and keys apply. A warning names gateway requests sharing the engine. Every
  run is recorded in the `benchmarks` table, and its payload and hardware file
  are kept in `~/.local/state/substrate/benchmarks/`.
- **Publishing:** `--validate` asks LocalMaxxing for its verdict without
  uploading. `--submit` uploads only runs it would mark verified (batch 1, the
  canonical prompt, complete evidence); `--force` uploads anyway. Uploads are
  recorded in the admin trail. A run is never uploaded twice: an upload whose
  only failure was lmx saving its receipt counts as done, and `bench submit`
  on a payload lmx already marked submitted records that run id instead of
  uploading again.
