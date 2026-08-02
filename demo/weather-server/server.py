"""A weather MCP server backed by the Open-Meteo public API.

No API key and no account: Open-Meteo serves forecasts anonymously. Two hosts
are involved, because a place name has to become coordinates before a forecast
can be asked for: geocoding-api.open-meteo.com resolves the name, and
api.open-meteo.com returns the forecast.
"""

import requests
from mcp.server.fastmcp import FastMCP

GEOCODING_URL = "https://geocoding-api.open-meteo.com/v1/search"
FORECAST_URL = "https://api.open-meteo.com/v1/forecast"

# Open-Meteo is usually well under a second. Ten seconds is generous enough for a
# slow network without leaving an agent waiting on a host that will never answer.
TIMEOUT = 10

MAX_FORECAST_DAYS = 16

# WMO 4677 present-weather codes, which Open-Meteo returns as a bare integer.
WEATHER_CODES = {
    0: "clear sky",
    1: "mainly clear",
    2: "partly cloudy",
    3: "overcast",
    45: "fog",
    48: "depositing rime fog",
    51: "light drizzle",
    53: "moderate drizzle",
    55: "dense drizzle",
    56: "light freezing drizzle",
    57: "dense freezing drizzle",
    61: "slight rain",
    63: "moderate rain",
    65: "heavy rain",
    66: "light freezing rain",
    67: "heavy freezing rain",
    71: "slight snowfall",
    73: "moderate snowfall",
    75: "heavy snowfall",
    77: "snow grains",
    80: "slight rain showers",
    81: "moderate rain showers",
    82: "violent rain showers",
    85: "slight snow showers",
    86: "heavy snow showers",
    95: "thunderstorm",
    96: "thunderstorm with slight hail",
    99: "thunderstorm with heavy hail",
}

mcp = FastMCP("weather")


class WeatherError(Exception):
    """A failure worth showing the caller verbatim."""


def _get(url, params):
    """GET and decode JSON, turning any transport failure into a message that
    names the host. A caller who cannot reach a host needs to know which one, so
    they can decide whether to allow it, retry, or give up."""
    try:
        response = requests.get(url, params=params, timeout=TIMEOUT)
        response.raise_for_status()
        return response.json()
    except requests.HTTPError as exc:
        raise WeatherError(
            f"{url} returned HTTP {exc.response.status_code}"
        ) from exc
    except requests.RequestException as exc:
        host = url.split("/")[2]
        raise WeatherError(f"could not reach {host}: {exc}") from exc
    except ValueError as exc:
        raise WeatherError(f"{url} returned a response that was not JSON") from exc


def _resolve(place):
    """Turn a place name into the first matching location, or raise."""
    data = _get(GEOCODING_URL, {"name": place, "count": 1, "format": "json"})
    results = data.get("results") or []
    if not results:
        raise WeatherError(
            f"no place called {place!r} was found. Try a city name, "
            "optionally with its country: 'Lagos', 'Springfield, US'."
        )
    return results[0]


def _describe(location):
    """A human label for a geocoding hit: city, region, country, as available."""
    parts = [location.get("name"), location.get("admin1"), location.get("country")]
    return ", ".join(p for p in parts if p)


def _units(fahrenheit):
    return {
        "temperature_unit": "fahrenheit" if fahrenheit else "celsius",
        "wind_speed_unit": "mph" if fahrenheit else "kmh",
        "precipitation_unit": "inch" if fahrenheit else "mm",
    }


def _symbols(fahrenheit):
    return ("°F", "mph", "in") if fahrenheit else ("°C", "km/h", "mm")


@mcp.tool()
def get_current_weather(place: str, fahrenheit: bool = False) -> str:
    """Get the weather happening right now at a place.

    Args:
        place: A city or place name, for example "Lagos" or "Springfield, US".
        fahrenheit: Report in Fahrenheit, miles per hour and inches instead of
            Celsius, km/h and millimetres.
    """
    try:
        location = _resolve(place)
        data = _get(
            FORECAST_URL,
            {
                "latitude": location["latitude"],
                "longitude": location["longitude"],
                "current": "temperature_2m,apparent_temperature,relative_humidity_2m,wind_speed_10m,precipitation,weather_code",
                "timezone": "auto",
                **_units(fahrenheit),
            },
        )
    except WeatherError as exc:
        return f"Could not get the weather for {place!r}: {exc}"

    current = data.get("current") or {}
    degree, speed, _ = _symbols(fahrenheit)
    condition = WEATHER_CODES.get(current.get("weather_code"), "unknown conditions")

    return (
        f"{_describe(location)}: {condition}, "
        f"{current.get('temperature_2m')}{degree} "
        f"(feels like {current.get('apparent_temperature')}{degree}), "
        f"humidity {current.get('relative_humidity_2m')}%, "
        f"wind {current.get('wind_speed_10m')} {speed}, "
        f"precipitation {current.get('precipitation')}. "
        f"Local time {current.get('time')}."
    )


@mcp.tool()
def get_forecast(place: str, days: int = 3, fahrenheit: bool = False) -> str:
    """Get a daily weather forecast for a place.

    Args:
        place: A city or place name, for example "Lagos" or "Springfield, US".
        days: How many days to forecast, 1 to 16. Defaults to 3.
        fahrenheit: Report in Fahrenheit and inches instead of Celsius and
            millimetres.
    """
    if days < 1 or days > MAX_FORECAST_DAYS:
        return f"days must be between 1 and {MAX_FORECAST_DAYS}, got {days}."

    try:
        location = _resolve(place)
        data = _get(
            FORECAST_URL,
            {
                "latitude": location["latitude"],
                "longitude": location["longitude"],
                "daily": "weather_code,temperature_2m_max,temperature_2m_min,precipitation_sum",
                "forecast_days": days,
                "timezone": "auto",
                **_units(fahrenheit),
            },
        )
    except WeatherError as exc:
        return f"Could not get a forecast for {place!r}: {exc}"

    daily = data.get("daily") or {}
    dates = daily.get("time") or []
    degree, _, depth = _symbols(fahrenheit)

    lines = [f"{days}-day forecast for {_describe(location)}:"]
    for i, date in enumerate(dates):
        condition = WEATHER_CODES.get(daily["weather_code"][i], "unknown conditions")
        lines.append(
            f"  {date}: {condition}, "
            f"{daily['temperature_2m_min'][i]} to {daily['temperature_2m_max'][i]}{degree}, "
            f"precipitation {daily['precipitation_sum'][i]}{depth}"
        )
    return "\n".join(lines)


@mcp.tool()
def find_place(place: str, limit: int = 5) -> str:
    """Look up the coordinates of a place name, and disambiguate between places
    that share one. Useful when a forecast came back for the wrong Springfield.

    Args:
        place: A place name to search for.
        limit: How many matches to return, 1 to 10. Defaults to 5.
    """
    limit = max(1, min(limit, 10))
    try:
        data = _get(GEOCODING_URL, {"name": place, "count": limit, "format": "json"})
    except WeatherError as exc:
        return f"Could not look up {place!r}: {exc}"

    results = data.get("results") or []
    if not results:
        return f"No place called {place!r} was found."

    lines = [f"Places matching {place!r}:"]
    for hit in results:
        lines.append(
            f"  {_describe(hit)} ({hit['latitude']:.4f}, {hit['longitude']:.4f})"
        )
    return "\n".join(lines)


if __name__ == "__main__":
    mcp.run()
