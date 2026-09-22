/** @type {import('next').NextConfig} */
const nextConfig = {
	// The Go API runs separately. Proxying through Next keeps the browser on one
	// origin, which avoids CORS entirely in development and means the deployed
	// frontend needs no knowledge of where the API lives.
	async rewrites() {
		return [
			{
				source: "/api/:path*",
				destination: `${process.env.ARGUS_API_URL ?? "http://localhost:8080"}/api/:path*`,
			},
		];
	},
};

export default nextConfig;
