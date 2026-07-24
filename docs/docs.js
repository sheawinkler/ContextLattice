(() => {
  const searchInput = document.querySelector("#docs-search");
  const searchResults = document.querySelector("#docs-search-results");
  const sidebar = document.querySelector(".docs-sidebar");
  const menuToggle = document.querySelector(".docs-menu-toggle");
  let documents = [];

  const closeResults = () => {
    if (!searchResults) return;
    searchResults.classList.remove("is-active");
    searchResults.replaceChildren();
  };

  const scoreDocument = (documentEntry, terms) => {
    const title = documentEntry.title.toLowerCase();
    const headings = documentEntry.headings.join(" ").toLowerCase();
    const content = `${documentEntry.summary} ${documentEntry.text}`.toLowerCase();
    return terms.reduce((score, term) => {
      if (title.includes(term)) return score + 8;
      if (headings.includes(term)) return score + 4;
      if (content.includes(term)) return score + 1;
      return score;
    }, 0);
  };

  const renderSearch = () => {
    if (!searchInput || !searchResults) return;
    const terms = searchInput.value
      .trim()
      .toLowerCase()
      .split(/\s+/)
      .filter(Boolean);
    if (!terms.length) {
      closeResults();
      return;
    }

    const matches = documents
      .map((documentEntry) => ({
        documentEntry,
        score: scoreDocument(documentEntry, terms),
      }))
      .filter((match) => match.score > 0)
      .sort((left, right) => right.score - left.score
        || left.documentEntry.title.localeCompare(right.documentEntry.title))
      .slice(0, 6);

    searchResults.replaceChildren();
    searchResults.classList.add("is-active");
    if (!matches.length) {
      const empty = document.createElement("p");
      empty.className = "docs-search-empty";
      empty.textContent = "No matching field notes.";
      searchResults.append(empty);
      return;
    }

    for (const { documentEntry } of matches) {
      const link = document.createElement("a");
      const title = document.createElement("strong");
      const summary = document.createElement("small");
      link.href = documentEntry.href;
      title.textContent = documentEntry.title;
      summary.textContent = documentEntry.summary;
      link.append(title, summary);
      searchResults.append(link);
    }
  };

  if (searchInput && searchResults) {
    fetch("/docs/search-index.json")
      .then((response) => (response.ok ? response.json() : Promise.reject()))
      .then((index) => {
        documents = Array.isArray(index.documents) ? index.documents : [];
      })
      .catch(() => {
        documents = [];
      });

    searchInput.addEventListener("input", renderSearch);
    document.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        searchInput.focus();
        searchInput.select();
      }
      if (event.key === "Escape") {
        closeResults();
        searchInput.blur();
      }
    });
    document.addEventListener("click", (event) => {
      if (!event.target.closest(".docs-search")) closeResults();
    });
  }

  if (sidebar && menuToggle) {
    menuToggle.addEventListener("click", () => {
      const open = sidebar.classList.toggle("is-open");
      menuToggle.setAttribute("aria-expanded", String(open));
    });
  }

  for (const frame of document.querySelectorAll(".code-frame")) {
    const label = frame.querySelector(".code-frame-label");
    const code = frame.querySelector("code");
    if (!label || !code) continue;
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = "Copy";
    button.setAttribute("aria-label", "Copy code block");
    button.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(code.textContent || "");
        button.textContent = "Copied";
      } catch {
        button.textContent = "Select";
      }
      window.setTimeout(() => {
        button.textContent = "Copy";
      }, 1400);
    });
    label.append(button);
  }
})();
