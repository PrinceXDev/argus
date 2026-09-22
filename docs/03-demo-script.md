# The three-minute demo

The video is worth more than the repository, because it is the only artifact every judge
will actually consume end to end. This is the storyboard the product was built to serve.

Record **beat 5 first**. If the retraction moment is not smooth, nothing else matters.

---

## Beats

| Time | Beat | On screen | Say |
| --- | --- | --- | --- |
| 0:00–0:15 | **The problem** | Split screen: a vector RAG answer with five retrieved chunks beneath it | "This answer is right. Can you tell me *why* it's right? Neither can it." |
| 0:15–0:30 | **The thesis** | Same question in ARGUS; hit derive | "ARGUS doesn't retrieve passages. It searches for the cheapest chain of sourced claims that entails an answer." |
| 0:30–1:00 | **The proof chain** | Chain animates step by step; source under each; running probability decaying | "Every hop is a claim with a source. The edge weight is −log of its confidence — so the *shortest* path is the *most likely* derivation. That's one `algo.SPpaths` call." |
| 1:00–1:15 | **Latency** | The where-the-time-went panel | "The reasoning took milliseconds. Everything else you waited for was the model reading it out." |
| 1:15–1:45 | **Abstention** ⭐ | Ask about Ghost Nominees Ltd. **INSUFFICIENT EVIDENCE**, frontier claims listed | "It stopped. It shows you exactly how far the evidence reaches and where it runs out. A vector database can't do this — it always has a top-k, so it always answers." |
| 1:45–2:15 | **Retraction** ⭐⭐ | Click *retract this fact* on the middle step. Panel: **load-bearing**, verdict collapses | "`GRAPH.COPY` forks the graph, we delete one fact, re-derive, and diff. Zero model calls — this is pure graph computation. The whole conclusion rested on one filing." |
| 2:15–2:35 | **The court** | Contested badge + *"six sources, one origin"* | "Max-flow from the origin publishers to the claim. Six sources assert it, but they carry the independent support of about two. The bottleneck says they share an upstream." |
| 2:35–2:55 | **The number** | `bench/results.md` grouped bar chart; point at L6 and L7 | "Five arms, same graph, same corpus, same model. On the unanswerable and poisoned questions every baseline confidently answers. We abstain." |
| 2:55–3:00 | **Close** | The one-box architecture diagram | "One database. Take FalkorDB out and there is no product left." |

---

## Why this order

The two starred beats are the ones a judge will still remember after fifty submissions, and
they are deliberately placed either side of the midpoint rather than at the end — a viewer
who stops at two minutes has already seen both.

**Abstention before retraction.** Refusing to answer is the more surprising moment but the
less spectacular one; it also establishes that the system has a notion of "not enough",
which is what makes "this fact was load-bearing" mean something thirty seconds later.

**Latency before either.** The panel is fifteen seconds and it converts "fast" from a claim
into an observation. It also pre-empts the obvious objection to the retraction demo — that
forking a graph per candidate must be slow.

**The benchmark last.** A number is only persuasive once the viewer knows what is being
measured. Shown first, the L6/L7 columns look like arbitrary categories; shown after the
abstention demo, they are obviously the thing they just watched.

---

## Recording notes

- **Fixture mode exists for this.** `make fixture` serves recorded data through the real
  request path, so the recording does not depend on a provider staying up mid-take. State
  in the video description which mode was recorded; `/api/health` reports `fixture: true`
  and the distinction must never be blurred.
- **Prefer the live path** if the instance is healthy. Real latency numbers are worth more
  than convenient ones.
- **Do not narrate the UI.** Say what the system is doing, not what the user is clicking.
- **One take per beat**, cut together. A single continuous take will lose the timing on the
  chain animation.

---

## The one sentence

If a judge remembers nothing else:

> **It searches for the cheapest chain of evidence that entails an answer — and when no such
> chain exists, it says so.**
