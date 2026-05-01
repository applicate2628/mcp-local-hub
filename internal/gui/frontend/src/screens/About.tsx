import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";

// Mirrors versionDTO in internal/gui/version.go. Keep in sync if the
// backend shape changes — there's a Go-side test asserting field
// names, but the TS shape isn't generated from it.
interface VersionInfo {
  version: string;
  commit: string;
  build_date: string;
  go_version: string;
  platform: string;
  homepage: string;
  issues: string;
  license: string;
  author: string;
}

type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: VersionInfo }
  | { status: "error"; error: string };

export function AboutScreen() {
  const [state, setState] = useState<LoadState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    fetchOrThrow<VersionInfo>("/api/version", "object")
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch((err: Error) => {
        if (!cancelled) setState({ status: "error", error: err.message });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (state.status === "loading") {
    return (
      <section class="about-screen" data-testid="about-loading">
        <h1>About</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.status === "error") {
    return (
      <section class="about-screen" data-testid="about-error">
        <h1>About</h1>
        <p class="error">Failed to load version info: {state.error}</p>
      </section>
    );
  }

  const v = state.data;
  return (
    <section class="about-screen" data-testid="about-loaded">
      <h1>About mcp-local-hub</h1>

      <dl class="about-meta">
        <dt>Version</dt>
        <dd data-testid="about-version">{v.version}</dd>

        <dt>Commit</dt>
        <dd data-testid="about-commit"><code>{v.commit}</code></dd>

        <dt>Build date</dt>
        <dd data-testid="about-build-date">{v.build_date}</dd>

        <dt>Go version</dt>
        <dd>{v.go_version}</dd>

        <dt>Platform</dt>
        <dd>{v.platform}</dd>

        <dt>License</dt>
        <dd>{v.license}</dd>

        <dt>Author</dt>
        <dd>{v.author}</dd>
      </dl>

      <h2>Links</h2>
      <ul class="about-links">
        <li>
          <a href={v.homepage} target="_blank" rel="noopener noreferrer">
            GitHub repository
          </a>
        </li>
        <li>
          <a href={v.issues} target="_blank" rel="noopener noreferrer">
            Issue tracker
          </a>
        </li>
      </ul>

      <h2>Documentation</h2>
      <ul class="about-links">
        {/* Doc links are derived from the canonical homepage URL so a
            future repo rename automatically follows. /blob/master/<path>
            is the GitHub URL convention; if a future release pins the
            doc viewer to a tag, this is the single place to change. */}
        <li>
          <a
            href={`${v.homepage}/blob/master/README.md`}
            target="_blank"
            rel="noopener noreferrer"
            data-testid="about-readme-link"
          >
            README
          </a>{" "}
          — overview, capabilities, supported clients
        </li>
        <li>
          <a
            href={`${v.homepage}/blob/master/INSTALL.md`}
            target="_blank"
            rel="noopener noreferrer"
            data-testid="about-install-link"
          >
            INSTALL
          </a>{" "}
          — installation walkthrough
        </li>
        <li>
          <a
            href={`${v.homepage}/blob/master/docs/phase-3b-ii-verification.md`}
            target="_blank"
            rel="noopener noreferrer"
            data-testid="about-verification-link"
          >
            Phase 3B-II verification guide
          </a>{" "}
          — manual smoke checklist for current GUI features
        </li>
      </ul>
    </section>
  );
}
