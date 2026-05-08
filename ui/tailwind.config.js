/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Datadog-ish palette: deep navy sidebar, teal accent,
        // amber for warnings, soft red for incidents.
        ink: {
          950: "#0b0d12",
          900: "#11141c",
          800: "#161a25",
          700: "#1d2230",
          600: "#262c3d",
          500: "#3a4257",
          400: "#5b657f",
          300: "#8590a8",
          200: "#b5bcca",
          100: "#dde0e8",
          50: "#f4f5f8",
        },
        accent: {
          DEFAULT: "#3b82f6",
          hover: "#60a5fa",
          subtle: "#172554",
        },
        good: "#3bb273",
        warn: "#f0a500",
        bad: "#e0533d",
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
        // Datadog admin pages run dense — most text is 12-13px.
        "2xs": ["11px", "16px"],
      },
    },
  },
  plugins: [],
};
