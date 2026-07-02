import { SourceMindMap } from "@/components/SourceMindMap";
import { TopicHierarchyConsole } from "@/components/TopicHierarchyConsole";

export default function MindMapPage() {
  return (
    <div className="cl-page cl-topic-visual-page">
      <TopicHierarchyConsole />
      <section className="cl-panel cl-visual-mindmap-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">visual graph</p>
            <h3>Rollup mind map</h3>
          </div>
          <span className="cl-badge">interactive</span>
        </div>
        <p className="cl-panel-note">
          Same memory graph, lower on the page: useful when you want the spatial shape after the hierarchy has done the sorting.
        </p>
        <SourceMindMap />
      </section>
    </div>
  );
}
