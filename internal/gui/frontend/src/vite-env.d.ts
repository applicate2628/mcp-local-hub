/// <reference types="vite/client" />

// TypeScript 6 tightened side-effect import resolution: a bare
// `import "./x.css"` now needs an ambient module declaration for the asset.
// Vite handles these at build time; declare them here so the type-checker
// accepts the CSS (and other static-asset) side-effect imports.
declare module "*.css";
declare module "*.svg";
declare module "*.png";
