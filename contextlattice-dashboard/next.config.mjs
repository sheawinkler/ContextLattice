const nextConfig = {
  reactStrictMode: true,
  env: {
    NEXT_PUBLIC_AUTH_REQUIRED:
      process.env.NEXT_PUBLIC_AUTH_REQUIRED ||
      (process.env.AUTH_REQUIRED === "true" ? "true" : "false"),
  },
};

export default nextConfig;
