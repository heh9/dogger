from aiohttp import web


async def handle(request):
    return web.Response(text="Hello, world")


if __name__ == "__main__":
    app = web.Application()
    app.router.add_get("/", handle)
    web.run_app(app)
