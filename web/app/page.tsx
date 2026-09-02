"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CourtPanel } from "@/components/CourtPanel";
import { Frontier } from "@/components/Frontier";
import { LatencyHud } from "@/components/LatencyHud";
import { ProofChain } from "@/components/ProofChain";
import { RetractionPanel } from "@/components/RetractionPanel";
import { VerdictBadge } from "@/components/VerdictBadge";
import {
	type Analysis,
	ask,
	type Dispute,
	type Evidence,
	type GraphStats,
	type Ruling,
	retract,
	stats,
	type Verdict,
} from "@/lib/api";

// Fixture mode has one recorded world and one recorded gap, so these two
// questions are the only ones that behave differently there. Against a real
// ingested corpus the whole generated question set works; `make questions`
// writes it out.
const EXAMPLES = [
	"Which entity ultimately controls Ashcroft Freight Corp?",
	"Who controls Ghost Nominees Ltd?",
];

export default function Page() {
	const [question, setQuestion] = useState("");
	const [leapBudget, setLeapBudget] = useState(6);
	const [minConfidence, setMinConfidence] = useState(0.35);
	const [adjudicate, setAdjudicate] = useState(true);

	const [busy, setBusy] = useState(false);
	const [verdict, setVerdict] = useState<Verdict | null>(null);
	const [evidence, setEvidence] = useState<Evidence[]>([]);
	const [ruling, setRuling] = useState<Ruling | null>(null);
	const [answerText, setAnswerText] = useState("");
	const [totalMs, setTotalMs] = useState<number | undefined>();
	const [error, setError] = useState<string | null>(null);

	const [analysis, setAnalysis] = useState<Analysis | null>(null);
	const [retracting, setRetracting] = useState<string | null>(null);

	const [graphStats, setGraphStats] = useState<GraphStats | null>(null);
	const [fixture, setFixture] = useState(false);
	const abortRef = useRef<AbortController | null>(null);

	useEffect(() => {
		stats().then(setGraphStats);
		// A viewer must always be able to tell which mode produced a result, so
		// fixture mode is announced rather than merely documented.
		fetch("/api/health")
			.then((r) => r.json())
			.then((h) => setFixture(Boolean(h.fixture)))
			.catch(() => {});
	}, []);

	const evidenceMap = useMemo(() => {
		const m = new Map<string, Evidence>();
		for (const e of evidence) m.set(e.claimId, e);
		return m;
	}, [evidence]);

	const disputeMap = useMemo(() => {
		const m = new Map<string, Dispute[]>();
		for (const d of ruling?.disputes ?? []) {
			m.set(d.claimId, [...(m.get(d.claimId) ?? []), d]);
		}
		return m;
	}, [ruling]);

	const submit = useCallback(
		async (q: string) => {
			const text = q.trim();
			if (!text || busy) return;

			abortRef.current?.abort();
			const ctrl = new AbortController();
			abortRef.current = ctrl;

			setBusy(true);
			setVerdict(null);
			setEvidence([]);
			setRuling(null);
			setAnswerText("");
			setAnalysis(null);
			setTotalMs(undefined);
			setError(null);

			try {
				await ask(
					{ question: text, leapBudget, minConfidence, adjudicate },
					{
						onVerdict: setVerdict,
						onEvidence: setEvidence,
						onRuling: setRuling,
						onAnswer: (t) => setAnswerText(t),
						onDone: (d) => setTotalMs(d.totalMs),
						onError: setError,
					},
					ctrl.signal,
				);
			} catch (e) {
				if ((e as Error).name !== "AbortError") setError((e as Error).message);
			} finally {
				setBusy(false);
			}
		},
		[busy, leapBudget, minConfidence, adjudicate],
	);

	const runRetraction = useCallback(
		async (claimId: string) => {
			if (!verdict || retracting) return;
			setRetracting(claimId);
			setError(null);
			try {
				setAnalysis(
					await retract({
						question: verdict.question,
						claimId,
						leapBudget,
						minConfidence,
					}),
				);
			} catch (e) {
				setError((e as Error).message);
			} finally {
				setRetracting(null);
			}
		},
		[verdict, retracting, leapBudget, minConfidence],
	);

	const best = verdict?.chains?.[0];

	return (
		<main className="mx-auto max-w-[860px] px-5 py-10">
			{fixture && (
				<div
					className="mb-6 rounded-lg border border-contested/50 bg-contested/5 px-3 py-2
                        font-mono text-[11px] text-contested"
				>
					fixture mode — recorded data, not a live graph. The request path is
					real; the database and the model are recorded.
				</div>
			)}

			<header className="mb-8">
				<div className="flex items-baseline justify-between gap-4">
					<h1 className="font-mono text-[15px] tracking-wide text-fg">
						ARGUS
						<span className="ml-2.5 font-sans text-[13px] font-normal text-muted">
							proof-carrying retrieval
						</span>
					</h1>
					{graphStats && (
						<span className="font-mono text-[11px] tabular-nums text-muted">
							{graphStats.nodeCount.toLocaleString()} nodes ·{" "}
							{graphStats.relCount.toLocaleString()} edges
						</span>
					)}
				</div>
				<p className="mt-2 max-w-[62ch] text-[14px] leading-relaxed text-muted">
					Ask a question and ARGUS searches for the cheapest chain of sourced
					claims that entails an answer. If no chain exists inside your evidence
					budget, it says so.
				</p>
			</header>

			{/* ── query ─────────────────────────────────────────────────────────── */}
			<section className="rounded-lg border border-line bg-panel p-4">
				<form
					onSubmit={(e) => {
						e.preventDefault();
						void submit(question);
					}}
				>
					<textarea
						value={question}
						onChange={(e) => setQuestion(e.target.value)}
						onKeyDown={(e) => {
							if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
								e.preventDefault();
								void submit(question);
							}
						}}
						rows={2}
						placeholder="Which entity ultimately controls…"
						className="w-full resize-none rounded border border-line bg-ink px-3 py-2.5
                       text-[15px] text-fg outline-none placeholder:text-muted/60
                       focus:border-accent/60"
					/>

					<div className="mt-3 grid gap-4 sm:grid-cols-2">
						<label className="block">
							<div className="flex items-baseline justify-between font-mono text-[11px] text-muted">
								<span>speculation budget</span>
								<span className="tabular-nums text-fg/70">
									{leapBudget} leaps
								</span>
							</div>
							<input
								type="range"
								min={1}
								max={12}
								value={leapBudget}
								onChange={(e) => setLeapBudget(Number(e.target.value))}
								className="mt-1.5 w-full accent-[var(--color-accent)]"
							/>
							<p className="mt-1 text-[11px] leading-snug text-muted/80">
								Lower demands more direct evidence, and abstains sooner.
							</p>
						</label>

						<label className="block">
							<div className="flex items-baseline justify-between font-mono text-[11px] text-muted">
								<span>confidence floor</span>
								<span className="tabular-nums text-fg/70">
									{(minConfidence * 100).toFixed(0)}%
								</span>
							</div>
							<input
								type="range"
								min={5}
								max={90}
								value={minConfidence * 100}
								onChange={(e) => setMinConfidence(Number(e.target.value) / 100)}
								className="mt-1.5 w-full accent-[var(--color-accent)]"
							/>
							<p className="mt-1 text-[11px] leading-snug text-muted/80">
								Below this, ARGUS declines rather than guessing.
							</p>
						</label>
					</div>

					<div className="mt-3 flex flex-wrap items-center gap-3">
						<button
							type="submit"
							disabled={busy || !question.trim()}
							className="rounded bg-accent px-4 py-1.5 font-mono text-[12px] text-white
                         transition-opacity hover:opacity-90 disabled:opacity-40"
						>
							{busy ? "deriving…" : "derive"}
						</button>
						<label className="flex items-center gap-1.5 font-mono text-[11px] text-muted">
							<input
								type="checkbox"
								checked={adjudicate}
								onChange={(e) => setAdjudicate(e.target.checked)}
								className="accent-[var(--color-accent)]"
							/>
							check for disputes
						</label>
						<span className="ml-auto font-mono text-[11px] text-muted/60">
							⌘↵
						</span>
					</div>
				</form>

				{!verdict && !busy && (
					<div className="mt-4 border-t border-line pt-3">
						<p className="font-mono text-[11px] text-muted">try</p>
						<div className="mt-1.5 flex flex-col gap-1">
							{EXAMPLES.map((ex) => (
								<button
									key={ex}
									onClick={() => {
										setQuestion(ex);
										void submit(ex);
									}}
									className="text-left text-[13px] text-muted transition-colors hover:text-accent"
								>
									{ex}
								</button>
							))}
						</div>
					</div>
				)}
			</section>

			{error && (
				<div className="mt-4 rounded-lg border border-critical/50 bg-critical/5 p-3 text-[13px] text-critical">
					{error}
				</div>
			)}

			{/* ── result ────────────────────────────────────────────────────────── */}
			{verdict && (
				<section className="mt-6 space-y-4">
					<div className="flex flex-wrap items-center gap-3">
						<VerdictBadge
							status={verdict.status}
							confidence={best?.confidence}
						/>
						{best && (
							<span className="font-mono text-[11px] tabular-nums text-muted">
								{best.hops} hops · {best.leaps} leaps
								{verdict.chains.length > 1 && (
									<>
										{" "}
										· {verdict.chains.length - 1} alternative derivation
										{verdict.chains.length > 2 ? "s" : ""}
									</>
								)}
							</span>
						)}
					</div>

					<LatencyHud timing={verdict.timing} totalMs={totalMs} />

					{best ? (
						<div className="rounded-lg border border-line bg-panel p-4">
							<h3 className="mb-4 font-mono text-[11px] uppercase tracking-wider text-muted">
								the derivation
							</h3>
							<ProofChain
								chain={best}
								evidence={evidenceMap}
								disputes={disputeMap}
								onRetract={runRetraction}
								retractingId={retracting}
							/>
						</div>
					) : (
						<Frontier claims={verdict.frontier ?? []} reason={verdict.reason} />
					)}

					{ruling && <CourtPanel ruling={ruling} />}
					{analysis && <RetractionPanel analysis={analysis} />}

					{answerText && (
						<div className="rounded-lg border border-line bg-panel p-4">
							<h3 className="mb-2 font-mono text-[11px] uppercase tracking-wider text-muted">
								analyst&rsquo;s note
							</h3>
							<div className="whitespace-pre-wrap text-[15px] leading-relaxed text-fg/90">
								{answerText}
							</div>
						</div>
					)}
				</section>
			)}

			<footer className="mt-12 border-t border-line pt-4 font-mono text-[11px] text-muted/70">
				Built on FalkorDB · the derivation is one{" "}
				<span className="text-muted">algo.SPpaths</span> call over
				−ln(confidence) ·{" "}
				<a href="/api/catalogue" className="underline hover:text-accent">
					every query this runs
				</a>
			</footer>
		</main>
	);
}
