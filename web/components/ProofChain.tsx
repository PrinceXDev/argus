"use client";

import type { Chain, Dispute, Evidence } from "@/lib/api";

/**
 * The proof chain.
 *
 * This component is the argument. Everything else on the page supports it.
 *
 * Three things are shown per step that a list of retrieved passages cannot
 * show: which claim follows from which, the cost of each inferential step, and
 * the running probability of the derivation as it accumulates. The chain is
 * rendered top-down as a single connected column precisely so that a reader can
 * see it is one derivation rather than a bag of related snippets.
 */
export function ProofChain({
	chain,
	evidence,
	disputes,
	onRetract,
	retractingId,
}: {
	chain: Chain;
	evidence: Map<string, Evidence>;
	disputes: Map<string, Dispute[]>;
	onRetract?: (claimId: string) => void;
	retractingId?: string | null;
}) {
	// Running confidence: the product of the step confidences so far. This is the
	// same quantity the database minimised as a sum of -ln, shown back as a
	// probability so a reader can watch it decay along the chain.
	let running = 1;

	return (
		<ol className="relative">
			{chain.steps.map((step, i) => {
				running *= i === 0 ? 1 : step.confidence;
				const ev = evidence.get(step.claimId);
				const disputed = disputes.get(step.claimId) ?? [];
				const isRoot = i === 0;

				return (
					<li
						key={step.claimId}
						className="step-in relative pl-8"
						style={{ animationDelay: `${i * 90}ms` }}
					>
						{/* Connector: the edge, labelled with what it cost. */}
						{i > 0 && (
							<div
								className="draw-line absolute left-[11px] -top-6 h-6 w-px bg-line"
								style={{ animationDelay: `${i * 90 - 40}ms` }}
								aria-hidden
							/>
						)}

						<span
							className={[
								"absolute left-0 top-1 grid h-[23px] w-[23px] place-items-center",
								"rounded-full border text-[11px] font-mono",
								isRoot
									? "border-accent/60 bg-accent/15 text-accent"
									: "border-line bg-panel-2 text-muted",
							].join(" ")}
						>
							{i + 1}
						</span>

						<div className="pb-6">
							<div
								className={[
									"rounded-lg border bg-panel p-3.5",
									disputed.length > 0 ? "border-contested/50" : "border-line",
								].join(" ")}
							>
								<p className="text-[15px] leading-relaxed text-fg">
									{step.text}
								</p>

								<div className="mt-2.5 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[11px] text-muted">
									{isRoot ? (
										<span className="rounded bg-accent/10 px-1.5 py-0.5 font-mono text-accent">
											grounded premise
										</span>
									) : (
										<span className="font-mono">
											step {(step.confidence * 100).toFixed(0)}% · w=
											{step.weight}
										</span>
									)}

									{ev && (
										<span className="truncate">
											<span className="text-fg/70">{ev.sourceName}</span>
											{ev.documentTitle && <> · {ev.documentTitle}</>}
										</span>
									)}

									<span className="ml-auto font-mono tabular-nums text-fg/80">
										P = {(running * 100).toFixed(1)}%
									</span>
								</div>

								{/* Running confidence as a bar, so the decay is visible without
                    reading the numbers. */}
								<div className="mt-2 h-[3px] w-full overflow-hidden rounded-full bg-line">
									<div
										className="h-full rounded-full bg-proven/70 transition-[width] duration-500"
										style={{ width: `${Math.max(2, running * 100)}%` }}
									/>
								</div>

								{disputed.map((d) => (
									<div
										key={`${d.counterSource.id}:${d.counterText}`}
										className="mt-2.5 rounded border border-contested/40 bg-contested/5 p-2 text-[12px]"
									>
										<span className="font-medium text-contested">Disputed</span>
										<span className="text-muted">
											{" "}
											by {d.counterSource.name}:{" "}
										</span>
										<span className="text-fg/85">{d.counterText}</span>
									</div>
								))}

								{onRetract && (
									<button
										type="button"
										onClick={() => onRetract(step.claimId)}
										disabled={!!retractingId}
										className="mt-2.5 rounded border border-line px-2 py-1 font-mono text-[11px]
                               text-muted transition-colors hover:border-critical/60
                               hover:text-critical disabled:opacity-40"
									>
										{retractingId === step.claimId
											? "retracting…"
											: "retract this fact"}
									</button>
								)}
							</div>
						</div>
					</li>
				);
			})}
		</ol>
	);
}
