// The packager prepends the shared Markdown as a JSON string named guide.
export function activate(ctx) {
  ctx.tools.register({
    name: "recall_guide",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    execute: async () => guide,
  });
  ctx.commands.register({
    name: "contextlattice",
    run: async (_args, io) => io.reply(guide),
  });
}
