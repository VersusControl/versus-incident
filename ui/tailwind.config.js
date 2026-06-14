/** @type {import('tailwindcss').Config} */

// Every color token resolves through a CSS variable (defined in
// index.css) so the light/dark theme switch is a pure variable swap —
// no per-component theme classes. Variables hold "R G B" triplets so
// Tailwind's <alpha-value> opacity modifiers keep working.
const v = (name) => `rgb(var(--${name}) / <alpha-value>)`;

export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Neutral scale. In dark: 50-400 = text (bright→faint),
        // 500-950 = fills (controls→sidebar). The light map mirrors the
        // same SEMANTICS (100 is always "primary text", 600 always a
        // subtle fill) with inverted lightness.
        ink: {
          950: v("ink-950"),
          900: v("ink-900"),
          800: v("ink-800"),
          700: v("ink-700"),
          600: v("ink-600"),
          500: v("ink-500"),
          400: v("ink-400"),
          300: v("ink-300"),
          200: v("ink-200"),
          100: v("ink-100"),
          50: v("ink-50"),
        },
        accent: {
          DEFAULT: v("accent"),
          hover: v("accent-hover"),
          subtle: v("accent-subtle"),
        },
        // Text links only. Fills keep `accent`; links need a brighter blue
        // in dark (accent on surface measures 4.1:1, link 6.9:1).
        link: v("link"),

        // Semantic surfaces — the elevation ladder. Dark: each level is
        // perceptibly LIGHTER than the one below; light: page is gray,
        // cards are white, raised is a soft gray.
        surface: {
          sunken: v("surface-sunken"),
          DEFAULT: v("surface"),
          raised: v("surface-raised"),
        },

        // Severity ramp. DEFAULT = text/icon tone (AA on the theme's
        // card surface at 11px); solid = saturated fill for rails.
        sev: {
          critical: { DEFAULT: v("sev-critical"), solid: v("sev-critical-solid") },
          high: { DEFAULT: v("sev-high"), solid: v("sev-high-solid") },
          warn: { DEFAULT: v("sev-warn"), solid: v("sev-warn-solid") },
          info: { DEFAULT: v("sev-info"), solid: v("sev-info-solid") },
          ok: { DEFAULT: v("sev-ok"), solid: v("sev-ok-solid") },
        },

        // Legacy aliases — existing call sites keep compiling; tones
        // resolve to the themed severity ramp.
        good: v("sev-ok"),
        warn: v("sev-warn"),
        bad: v("sev-critical"),
      },
      zIndex: {
        sticky: "10",
        dropdown: "20",
        overlay: "40",
        modal: "50",
        toast: "100",
      },
      boxShadow: {
        card: "0 1px 2px rgb(0 0 0 / 0.25)",
        overlay: "0 8px 24px rgb(0 0 0 / 0.35)",
        modal: "0 16px 48px rgb(0 0 0 / 0.45)",
      },
      borderRadius: {
        card: "8px",
        control: "6px",
      },
      fontFamily: {
        sans: [
          "Inter",
          "ui-sans-serif",
          "system-ui",
          "-apple-system",
          "Segoe UI",
          "sans-serif",
        ],
        mono: [
          "JetBrains Mono",
          "ui-monospace",
          "SFMono-Regular",
          "Menlo",
          "monospace",
        ],
      },
      fontSize: {
        // Dense admin scale — most desktop text is 12-13px.
        "2xs": ["11px", "16px"],
      },
    },
  },
  plugins: [],
};
