import type { GraphQLTransport } from "./linear";

const LINEAR_GRAPHQL_URL = "https://api.linear.app/graphql";

interface GraphQLResponse {
	data?: unknown;
	errors?: { message: string }[];
}

/**
 * Real Linear GraphQL transport — the one piece that holds the host-only
 * `LINEAR_API_KEY` (ADR-0001). Linear authenticates with the raw key in the
 * Authorization header (no "Bearer" prefix). Kept thin and out of the unit
 * suite; the {@link createLinearClient} logic is exercised with a fake transport.
 */
export function createLinearTransport(apiKey: string): GraphQLTransport {
	return async (query, variables) => {
		const res = await fetch(LINEAR_GRAPHQL_URL, {
			method: "POST",
			headers: {
				"content-type": "application/json",
				authorization: apiKey,
			},
			body: JSON.stringify({ query, variables }),
		});
		if (!res.ok) {
			throw new Error(`Linear HTTP ${res.status}: ${await res.text()}`);
		}
		const json = (await res.json()) as GraphQLResponse;
		if (json.errors?.length) {
			throw new Error(`Linear GraphQL error: ${json.errors.map((e) => e.message).join("; ")}`);
		}
		return json.data;
	};
}
