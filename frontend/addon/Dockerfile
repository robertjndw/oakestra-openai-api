# syntax=docker/dockerfile:1
#
# Build contexts:
#   - the default context is this frontend/addon/ folder
#   - the named context "dashboard" is a checkout of the dashboard (dashboard-v2); the addon links
#     its SDK and UI packages from source
#
#   docker buildx build --build-context dashboard=../../../dashboard-v2 \
#     --build-arg ADDON_NAME=openai-api -t openai-api-addon-ui .
#
# The checkouts are laid out as /work/dashboard-v2 and /work/oakestra-openai-api/frontend/addon
# because package.json links the dashboard packages by the relative path
# ../../../dashboard-v2/packages/*.

# The output is static files, so build once on the host's platform rather than under emulation.
FROM --platform=$BUILDPLATFORM node:25-slim AS build
RUN npm install --global pnpm@12.16.1
WORKDIR /work

# Host node_modules and dist would be wrong for this platform (and huge), so keep them out.
COPY --from=dashboard --exclude=**/node_modules --exclude=**/dist . dashboard-v2/
RUN --mount=type=cache,id=pnpm,target=/root/.local/share/pnpm/store \
    cd dashboard-v2 && pnpm install --frozen-lockfile \
      --filter "@oakestra/dashboard-sdk..." --filter "@oakestra/ui..."

WORKDIR /work/oakestra-openai-api/frontend/addon
COPY package.json pnpm-lock.yaml ./
RUN --mount=type=cache,id=pnpm,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile

# The name is baked into the Module Federation container, so it is a build argument.
ARG ADDON_NAME=openai-api
ENV ADDON_NAME=${ADDON_NAME}
COPY . .
RUN pnpm build

# Addon UI contract: plain HTTP on port 80, reachable as addonui-<ADDON_NAME> on the dashboard
# network.
FROM nginx:1.30-alpine
# Only these two are substituted into the template, so nginx's own $uri, $1 etc. survive.
ENV API_UPSTREAM=http://root_ooa:8081 \
    NGINX_ENTRYPOINT_LOCAL_RESOLVERS=1 \
    NGINX_ENVSUBST_FILTER="API_UPSTREAM|NGINX_LOCAL_RESOLVERS"
COPY docker/default.conf.template /etc/nginx/templates/default.conf.template
COPY --from=build /work/oakestra-openai-api/frontend/addon/dist /usr/share/nginx/html
EXPOSE 80
