# claude-code-2.1.311 hook payload fixtures (TASK-0029)

Field names and enum-like values only; every free-text value is a placeholder.
Provenance of each file (honest account):

| File | Source |
|------|--------|
| UserPromptExpansion.json | Hand-restored from the field names and values printed on screen during the run. The original capture file was lost when the Worker deleted the capture directory with a wildcard rm. The verifier independently re-ran a `/skill` call and confirmed the field SET matches. |
| All other files | Scrubbed copies of original hook captures from real `claude -p` 2.1.311 runs (calls 7-8 of TASK-0029). The originals were deleted together with the temporary work directory after the fixtures were generated, so they cannot be re-checked against the originals now. |

Not confirmed by the verifier's independent re-run unless stated: single-run observations.
