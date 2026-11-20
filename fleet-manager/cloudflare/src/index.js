// One Durable Object owns the one fleet-manager container and forwards every request to it. A
// deploy does not restart a running durable_object container, so this replaces it whenever the
// deployed image or its environment differs from what it was started with.
import { DurableObject } from "cloudflare:workers";

const PORT = 8080;
const READY_WITHIN_MS = 30_000;
const IDLE_MS = 30 * 60 * 1000;
// What deploy.sh stores as Worker secrets; nothing else reaches the container.
const CONTAINER_ENV = [
	"FLEET_MANAGER_BUCKET",
	"FLEET_MANAGER_ACCOUNT",
	"FLEET_MANAGER_R2_JURISDICTION",
	"FLEET_MANAGER_R2_ACCESS_KEY_ID",
	"FLEET_MANAGER_R2_SECRET_ACCESS_KEY",
	"FLEET_MANAGER_DEFAULT_ALLOW_QUESMA_ETL",
	"FLEET_MANAGER_DEFAULT_TELEMETRY_COLLECTOR_URL",
];

export default {
	fetch(request, env) {
		return env.FLEET_MANAGER.getByName("fleet-manager").fetch(request);
	},
};

export class FleetManager extends DurableObject {
	async fetch(request) {
		this.booting ??= this.boot().finally(() => {
			this.booting = undefined;
		});
		try {
			await this.booting;
		} catch (err) {
			return new Response(`${err.message}\n`, { status: 503, headers: { "content-type": "text/plain" } });
		}
		// The container port speaks plain HTTP; this hop never leaves Cloudflare.
		const url = new URL(request.url);
		url.protocol = "http:";
		return this.ctx.container.getTcpPort(PORT).fetch(new Request(url, request));
	}

	async boot() {
		const container = this.ctx.container;
		const image = container.images.base;
		const env = { FLEET_MANAGER_PROVIDER: "r2", PORT: String(PORT) };
		for (const name of CONTAINER_ENV) {
			if (this.env[name]) env[name] = this.env[name];
		}
		const config = await sha256(JSON.stringify(env));
		const wanted = `${image} ${config}`;
		if (container.running) {
			if (this.started === wanted) return;
			const live = await container.inspect();
			if (live && live.image === image && live.labels.config === config) {
				// The idle timeout belongs to the Durable Object instance that set it, not the container.
				await container.setInactivityTimeout(IDLE_MS);
				this.started = wanted;
				return;
			}
			await container.destroy();
		}
		this.started = undefined;
		const began = Date.now();
		console.log(`fleet-manager container starting from ${image}`);
		container.start({ image, env, enableInternet: true, instance: "lite", labels: { config } });
		await container.setInactivityTimeout(IDLE_MS);
		const port = container.getTcpPort(PORT);
		const deadline = Date.now() + READY_WITHIN_MS;
		while (Date.now() < deadline) {
			if (!container.running) break;
			try {
				if ((await port.fetch("http://fleet-manager/healthz")).ok) {
					this.started = wanted;
					console.log(`fleet-manager container ready in ${Date.now() - began} ms`);
					return;
				}
			} catch {}
			await new Promise((resolve) => setTimeout(resolve, 250));
		}
		const exited = !container.running;
		await container.destroy().catch(() => {});
		throw new Error(
			exited
				? "fleet-manager exited during startup; its log is in the Cloudflare dashboard under Containers"
				: `fleet-manager did not answer /healthz on port ${PORT} within ${READY_WITHIN_MS / 1000} s`,
		);
	}
}

async function sha256(text) {
	const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
	return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
