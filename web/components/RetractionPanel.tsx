"use client";

import type { Analysis, Retraction } from "@/lib/api";

const IMPACT: Record<Retraction["impact"], { label: string; cls: string }> = {
	critical: {
		label: "load-bearing",
		cls: "border-critical/50 bg-critical/10 text-critical",
	},
	major: {
		label: "major",
		cls: "border-contested/50 bg-contested/10 text-contested",
	},
	minor: { label: "minor", cls: "border-line bg-panel-2 text-muted" },
	none: {
		label: "corroborated",
		cls: "border-proven/40 bg-proven/5 text-proven",
	},
};

/**
 * Counterfactual retraction results.
 *
 * The headline is the sentence at the top: which single fact the whole
 * conclusion rests on. Everything below it is the working.
 *
 * The "no model calls" line is not a boast about efficiency. It is the reason
 * the numbers mean something: each row is a re-derivation over a forked graph,
 * not a language model's opinion about how important a passage felt.
 */
export function RetractionPanel({ analysis }: { analysis: Analysis }) {
	const critical = analysis.retractions.filter((r) => r.collapses);

	return (
		<div className="rounded-lg border border-line bg-panel p-4">
			<div className="flex items-baseline justify-between gap-4">
				<h3 className="font-mono text-[11px] uppercase tracking-wider text-muted">
					what is this resting on?
				</h3>
				<span className="font-mono text-[11px] tabular-nums text-muted">
					{analysis.forksUsed} graph forks · {analysis.graphMs}ms ·{" "}
					{analysis.llmCalls} model calls
				</span>
			</div>

			{critical.length > 0 ? (
				<p className="mt-3 rounded border border-critical/40 bg-critical/5 p-3 text-[14px] leading-relaxed">
					<span className="font-medium text-critical">
						{critical.length === 1
							? "This conclusion rests on a single fact."
							: `This conclusion rests on ${critical.length} facts.`}
					</span>{" "}
					<span className="text-fg/80">
						Retract {critical.length === 1 ? "it" : "any of them"} and ARGUS
						would decline to answer.
					</span>
				</p>
			) : (
				<p className="mt-3 rounded border border-proven/40 bg-proven/5 p-3 text-[14px] text-fg/80">
					<span className="font-medium text-proven">
						No single point of failure.
					</span>{" "}
					Every claim on this chain has an alternative route to the same
					conclusion.
				</p>
			)}

			<ul className="mt-3 space-y-2">
				{analysis.retractions.map((r) => {
					const style = IMPACT[r.impact];
					const dropPct = analysis.baseConfidence
						? (r.delta / analysis.baseConfidence) * 100
						: 0;
					return (
						<li
							key={r.claimId}
							className="rounded border border-line bg-panel-2 p-3"
						>
							<div className="flex items-start justify-between gap-3">
								<p className="text-[13px] leading-relaxed text-fg/85">
									{r.text}
								</p>
								<span
									className={`shrink-0 rounded border px-1.5 py-0.5 font-mono text-[10px] ${style.cls}`}
								>
									{style.label}
								</span>
							</div>

							<div className="mt-2 flex items-center gap-3 font-mono text-[11px] tabular-nums text-muted">
								<span>
									{(analysis.baseConfidence * 100).toFixed(0)}% →{" "}
									<span
										className={r.collapses ? "text-critical" : "text-fg/80"}
									>
										{r.collapses
											? "abstains"
											: `${(r.confidence * 100).toFixed(0)}%`}
									</span>
								</span>
								{!r.collapses && r.delta > 0 && (
									<span>−{dropPct.toFixed(0)}%</span>
								)}
								{!r.collapses && r.alternativeHops > 0 && (
									<span className="text-muted">
										alternative route: {r.alternativeHops} hops
									</span>
								)}
							</div>
						</li>
					);
				})}
			</ul>
		</div>
	);
}
