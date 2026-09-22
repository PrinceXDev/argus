import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
	title: "ARGUS — proof-carrying retrieval",
	description:
		"Answers that carry their proof, and know when to stay silent. Built on FalkorDB.",
};

export default function RootLayout({
	children,
}: {
	children: React.ReactNode;
}) {
	return (
		<html lang="en">
			<body className="min-h-screen antialiased">{children}</body>
		</html>
	);
}
