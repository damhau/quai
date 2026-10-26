// Stroke icons (24x24 grid, Lucide-style) kept inline so the app has no icon dependency.
const PATHS: Record<string, string> = {
  server: "M4 4h16v6H4zM4 14h16v6H4zM8 7h.01M8 17h.01",
  database: "M12 3c4.4 0 8 1.3 8 3s-3.6 3-8 3-8-1.3-8-3 3.6-3 8-3zM4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6",
  bucket: "M4 7h16l-2 13H6L4 7zM3 7c0-1.7 4-3 9-3s9 1.3 9 3",
  box: "M21 8l-9-5-9 5v8l9 5 9-5V8zM3 8l9 5 9-5M12 13v8",
  rocket: "M5 15c-1.5 1.3-2 5-2 5s3.7-.5 5-2M12 15l-3-3M9 12a22 22 0 0 1 11-9 22 22 0 0 1-9 11M9 12H5l2-4h5M12 15v4l4-2v-5",
  terraform: "M9 3l6 3.5v7L9 10zM16 7.5l5-3v7l-5 3zM3 7v7l5 3v-7zM9 14l6 3.5V21l-6-3.5z",
  code: "M16 18l6-6-6-6M8 6l-6 6 6 6",
  grid: "M3 3h7v7H3zM14 3h7v7h-7zM14 14h7v7h-7zM3 14h7v7H3z",
  check: "M20 6L9 17l-5-5",
  x: "M18 6L6 18M6 6l12 12",
  clock: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2",
  git: "M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9",
  commit: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM3 12h6M15 12h6",
  inbox: "M22 12h-6l-2 3h-4l-2-3H2M5.5 5h13L22 12v6a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-6z",
  list: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  layers: "M12 2l10 5-10 5L2 7zM2 17l10 5 10-5M2 12l10 5 10-5",
  pen: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
  edit: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
  trash: "M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6",
  maximize: "M8 3H3v5M21 8V3h-5M16 21h5v-5M3 16v5h5",
  power: "M18.4 6.6a9 9 0 1 1-12.8 0M12 2v10",
  "hard-drive": "M22 12H2M5.5 5h13L22 12v6a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2v-6zM6 16h.01M10 16h.01",
  "arrow-up": "M12 19V5M5 12l7-7 7 7",
  search: "M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.3-4.3",
  plus: "M12 5v14M5 12h14",
  shield: "M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z",
  user: "M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8z",
  play: "M5 3l14 9-14 9z",
  refresh: "M23 4v6h-6M1 20v-6h6M3.5 9a9 9 0 0 1 14.9-3.4L23 10M1 14l4.6 4.4A9 9 0 0 0 20.5 15",
  file: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6",
  zap: "M13 2L3 14h9l-1 8 10-12h-9z",
  network: "M12 2v6M5 22v-4h14v4M12 8a3 3 0 1 0 0 0zM12 14v4M5 18h0M19 18h0",
  activity: "M22 12h-4l-3 9L9 3l-3 9H2",
  alert: "M10.3 3.9L1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0zM12 9v4M12 17h.01",
  chevron: "M9 18l6-6-6-6",
  save: "M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2zM17 21v-8H7v8M7 3v5h8",
};

export function Icon({ name, size = 16, className, style }: { name?: string; size?: number; className?: string; style?: React.CSSProperties }) {
  const d = PATHS[name ?? ""] ?? PATHS.grid;
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" className={className} style={style} aria-hidden="true">
      {d.split(/(?=M)/).map((p, i) => (
        <path key={i} d={p} />
      ))}
    </svg>
  );
}
