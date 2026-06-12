import assert from "node:assert/strict";
import test from "node:test";
import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { RetrievalPanel } from "@/components/RetrievalPanel";

test("RetrievalPanel renders async lane visibility and controls", () => {
  const html = renderToStaticMarkup(<RetrievalPanel />);

  assert.match(html, /Retrieval flow/);
  assert.match(html, /Async lane/);
  assert.match(html, /Project/);
  assert.match(html, /Mode/);
  assert.match(html, /Continuation events/);
  assert.match(html, /Stop continuation stream/);
});
