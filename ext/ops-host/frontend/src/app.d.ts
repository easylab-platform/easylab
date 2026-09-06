declare module '*.css' {
  const content: string
  export default content
}

interface ImportMetaEnv {
  readonly ZERGX_SERVICE: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
