const assert = require("node:assert/strict");
const fs = require("node:fs");

(async () => {
  const source = fs.readFileSync("static/repository-review.js", "utf8");
  const {reviewRoute, reviewURL, fullFileVersion, fileStatus, changeCounts, historyHasPages, largeDiff, mount} =
    await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
  const original = "https://workspace.example.test/example/?unrelated=kept#codex";
  const route = {
    repository: "repository-id", review: "frozen-review", snapshot: "", kind: "", commit: "a".repeat(40),
    file: "file-id", view: "file", version: "old", layout: "unified",
    line: {side: "old", number: 12000},
  };
  const href = reviewURL(original, route);
  const parsed = new URL(href);
  assert.equal(parsed.searchParams.get("unrelated"), "kept");
  assert.equal(parsed.searchParams.get("tab"), "repositories");
  assert.equal(parsed.hash, "#old-L12000");
  assert.deepEqual(reviewRoute(href), route);
  assert.equal(reviewRoute(original).layout, "unified");
  assert.equal(reviewRoute(original, "split").layout, "split");
  assert.equal(largeDiff({additions: 1999, deletions: 1}), false);
  assert.equal(largeDiff({additions: 2000, deletions: 1}), true);
  assert.equal(largeDiff({additions: null, deletions: null}), false);
  assert.equal(reviewRoute(original + "-L1").line, null);
  for (const bad of ["#old-L0", "#old-L-1", "#old-L1x", "#new-L9007199254740992"]) {
    assert.equal(reviewRoute("https://workspace.example.test/" + bad).line, null);
  }
  const overview = reviewURL(href);
  for (const key of ["repository", "review", "snapshot", "kind", "commit", "file", "view", "layout", "version"]) {
    assert.equal(new URL(overview).searchParams.has(key), false, key);
  }
  assert.equal(new URL(overview).hash, "");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "000000"}, "new"), "old");
  assert.equal(fullFileVersion({oldMode: "000000", newMode: "100644"}, "old"), "new");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "100644"}, ""), "new");
  assert.equal(fullFileVersion({oldMode: "100644", newMode: "100644"}, "old"), "old");
  assert.deepEqual(fileStatus("R100"), ["renamed", "Renamed"]);
  assert.deepEqual(fileStatus("T"), ["type", "Type changed"]);
  assert.equal(changeCounts({additions: null, deletions: null}), "Binary");
  assert.equal(changeCounts({additions: null, deletions: null, limited: true}), "Not compared");
  const worktreeURL = reviewURL(original, {repository: "repository-id", snapshot: "ephemeral-id", kind: "unstaged", file: "file-id"});
  assert.equal(reviewRoute(worktreeURL).snapshot, "ephemeral-id");
  assert.equal(reviewRoute(worktreeURL).review, "");
  assert.equal(reviewRoute(worktreeURL).kind, "unstaged");
  for (const total of [0, 1, 50, 51, 101]) {
    const pages = Math.max(1, Math.ceil(total / 50));
    for (let page = 0; page < pages; page++) {
      assert.equal(historyHasPages({page, hasMore: page + 1 < pages}), pages > 1);
    }
  }
  assert.equal(changeCounts({files: 3, additions: 7, deletions: 2, binaryFiles: 1}, true),
    "3 changed files · +7 · −2 · 1 binary file");
  assert.equal(changeCounts({files: 2, additions: 0, deletions: 0, limitedFiles: 1, lineCountsIncomplete: true}, true),
    "2 changed files · 1 not compared");

  class FakeElement {
    constructor(tag, className = "", textContent = "") {
      this.tag = tag; this.className = className; this.textContent = textContent;
      this.childNodes = []; this.dataset = {}; this.parentNode = null;
      this.classList = {
        contains: name => this.className.split(" ").includes(name),
        add: name => { if (!this.classList.contains(name)) this.className += " " + name; },
        remove: name => { this.className = this.className.split(" ").filter(part => part !== name).join(" "); },
      };
    }
    append(...children) {
      for (const child of children) { child.remove(); this.childNodes.push(child); child.parentNode = this; }
    }
    prepend(child) { child.remove(); this.childNodes.unshift(child); child.parentNode = this; }
    remove() {
      if (!this.parentNode) return;
      this.parentNode.childNodes.splice(this.parentNode.childNodes.indexOf(this), 1);
      this.parentNode = null;
    }
    replaceChildren(...children) {
      for (const child of this.childNodes) child.parentNode = null;
      this.childNodes = []; this.append(...children);
    }
    querySelector(selector) {
      if (selector === ".section-heading span") return this.querySelector(".section-heading")?.querySelector("span") || null;
      if (selector.startsWith(":scope > ")) return this.childNodes.find(child => child.matches(selector.slice(9))) || null;
      for (const child of this.childNodes) {
        if (child.matches(selector)) return child;
        const nested = child.querySelector(selector); if (nested) return nested;
      }
      return null;
    }
    querySelectorAll(selector) {
      const result = [];
      for (const child of this.childNodes) {
        if (child.matches(selector)) result.push(child);
        result.push(...child.querySelectorAll(selector));
      }
      return result;
    }
    matches(selector) {
      if (selector === "[data-repository-id]") return Boolean(this.dataset.repositoryId);
      if (selector.startsWith(".")) return selector.slice(1).split(".").every(name => this.classList.contains(name));
      return selector === this.tag;
    }
    addEventListener() {}
    get innerHTML() { return this._html || ""; }
    set innerHTML(html) {
      this._html = html;
      const warning = /<p class="notice warning">([^<]*)<\/p>/.exec(html)?.[1];
      const heading = new FakeElement("div", "section-heading");
      heading.append(new FakeElement("span", "", "1"));
      const grid = new FakeElement("div", "repo-grid");
      if (html.includes('data-repository-id="stored"')) grid.append(repositoryCard());
      this.replaceChildren(...(warning ? [new FakeElement("p", "notice warning", warning)] : []), heading, grid);
    }
  }
  const repositoryCard = () => {
    const card = new FakeElement("article", "repo-card"); card.dataset.repositoryId = "stored";
    const status = new FakeElement("div");
    const branch = new FakeElement("button"), refresh = new FakeElement("button");
    const actions = new FakeElement("div", "repository-review-actions"); actions.append(branch, refresh);
    const headNotice = new FakeElement("p", "repository-head-change"); headNotice.hidden = true;
    const history = new FakeElement("details", "repository-history");
    const commits = new FakeElement("div");
    history.append(new FakeElement("summary", "", "Local commits"), commits);
    card.append(status, actions, headNotice, history);
    card.querySelector = selector => ({
      "[data-repository-status]": status,
      "[data-review-branch]": branch,
      "[data-review-refresh]": refresh,
      "[data-repository-commits]": commits,
      ".repository-head-change": headNotice,
      ".repository-history": history,
    })[selector] || null;
    return card;
  };
  const previous = Object.fromEntries(["document", "location", "localStorage", "addEventListener", "removeEventListener"]
    .map(name => [name, globalThis[name]]));
  globalThis.document = {
    querySelector: () => ({}), createElement: tag => new FakeElement(tag),
    addEventListener() {}, removeEventListener() {},
  };
  globalThis.location = {href: "https://workspace.example.test/example/?tab=repositories"};
  globalThis.localStorage = {getItem: () => null};
  globalThis.addEventListener = () => {};
  globalThis.removeEventListener = () => {};
  let mounted;
  try {
    const element = new FakeElement("section");
    element.innerHTML = '<div class="section-heading"><span>1</span></div><div class="repo-grid"><article data-repository-id="stored"></article></div>';
    mounted = mount({slug: "example", element});
    const overview = element.childNodes[0];
    const originalCard = overview.querySelectorAll("[data-repository-id]")[0];
    const retainedHistory = originalCard.querySelector(".repository-history");
    retainedHistory.open = true;
    const detailsHTML = warning => `${warning ? `<p class="notice warning">${warning}</p>` : ""}<div class="section-heading"><span>1</span></div><div class="repo-grid"><article data-repository-id="stored"></article></div>`;
    mounted.updateHTML(detailsHTML("Some live worktrees could not be verified: first failure"));
    assert.equal(overview.querySelector(":scope > .notice.warning")?.textContent,
      "Some live worktrees could not be verified: first failure");
    assert.equal(overview.querySelectorAll("[data-repository-id]")[0], originalCard);
    assert.equal(originalCard.querySelector(".repository-history"), retainedHistory);
    assert.equal(retainedHistory.open, true);
    assert.equal(overview.querySelector(".section-heading span").textContent, "1");
    mounted.updateHTML(detailsHTML("Some live worktrees could not be verified: second failure"));
    assert.equal(overview.childNodes.filter(child => child.matches(".notice.warning")).length, 1);
    assert.equal(overview.querySelector(":scope > .notice.warning")?.textContent,
      "Some live worktrees could not be verified: second failure");
    mounted.updateHTML(detailsHTML(""));
    assert.equal(overview.querySelector(":scope > .notice.warning"), null);
  } finally {
    mounted?.destroy();
    for (const [name, value] of Object.entries(previous)) {
      if (value === undefined) delete globalThis[name]; else globalThis[name] = value;
    }
  }
  console.log("Repository URL, file-version, metadata and mounted warning contracts passed.");
})().catch(error => { console.error(error); process.exitCode = 1; });
