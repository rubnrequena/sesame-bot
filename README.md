```# 🕰️ Sesame Time Bot

This bot automates the process of logging attendance (clocking IN or OUT) on the Sesame Time application. It runs as a background scheduler, checking the current time against predefined schedules and performing necessary actions using browser automation.

## 🚀 Features

*   **Automated Scheduling:** Executes actions only at specified times (`HH:MM`).
*   **Flexible Scheduling:** Supports both general weekly schedules and day-specific overrides.
*   **Day Control:** Can be configured to run only on weekdays or on weekends.
*   **Robust Automation:** Handles the entire workflow: Login $\rightarrow$ Action Click $\rightarrow$ Logout.

## ⚙️ Configuration & Setup

This project relies heavily on environment variables. Before running, you must ensure a `.env` file (or system environment variables) contains the necessary credentials and schedule details.

### 1. Required Environment Variables

The bot *requires* the following credentials:

| Variable | Description | Example Value |
| :--- | :--- | :--- |
| `SESAME_EMAIL` | Your work email address for logging in. | `user@example.com` |
| `SESAME_PASSWORD` | Your password for the Sesame Time application. | `your_secure_password` |
| `HOURS_IN` | **Mandatory.** Comma-separated list of *general* time slots for clocking IN (e.g., `08:00, 09:30`). | `08:00,17:00` |
| `HOURS_OUT` | **Mandatory.** Comma-separated list of *general* time slots for clocking OUT. | `18:00` |

### 2. Optional Environment Variables

These variables control the behavior and schedule of the bot:

| Variable | Description | Type | Default/Notes |
| :--- | :--- | :--- | :--- |
| `HEADLESS` | Runs the browser in the background (headless mode). | String | Set to `"false"` to watch the automation process run visually. |
| `WEEKEND` | Controls execution on Saturday/Sunday. | String | Set to `"false"` to prevent running on weekends. |
| `MONDAY_IN`, `MONDAY_OUT`, etc. | Overrides for specific days. Use `DAYNAME_ACTION` (e.g., `WEDNESDAY_IN`). | String | Takes precedence over `HOURS_IN`/`HOURS_OUT`. |

**Scheduling Notes:**
*   Times in all scheduling variables must be in `HH:MM` format.
*   Multiple times for the same action/day should be separated by commas (e.g., `"08:00,12:00"`).

## 🛠️ How It Works (Execution Flow)

The bot operates as a continuous scheduler:

1.  **Initialization:** Reads and parses all necessary credentials and schedules from the environment.
2.  **Scheduler Loop:** Enters a loop, checking the system time every 30 seconds.
3.  **Schedule Check:** Compares the current date/time against the loaded schedule for the current day, respecting any day-specific overrides.
4.  **Action Trigger:** If the current time matches a scheduled time, it executes the `runAction` sequence.
5.  **`runAction` Details:**
    *   Launches a Chrome browser instance (headless by default).
    *   Navigates to the specified login URL (`https://app.sesametime.com/login`).
    *   Authenticates using the provided credentials.
    *   Locates and clicks the button corresponding to the action (`Entrar` for IN, `Salir` for OUT).
    *   Pauses for 5 seconds (a simulated buffer period).
    *   Logs out of the session to complete the cycle.

## 🐛 Troubleshooting & Debugging

If the bot fails to clock in or out, check these items:

1.  **Credentials:** Verify `SESAME_EMAIL` and `SESAME_PASSWORD` are correctly set in the environment.
2.  **Selectors:** The bot relies on specific CSS selectors (e.g., `#btn-next-login`, `.headerProfileName`). If Sesame Time updates its web interface, these selectors in `main.go` must be updated manually.
3.  **Mandatory Variables:** Ensure both `HOURS_IN` AND `HOURS_OUT` are set in the environment, as required by the current validation logic.
