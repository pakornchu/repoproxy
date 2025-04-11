# repoproxy

Very dirty YUM repository proxy.

## Dependencies
1. PostgreSQL database

## Quick start
1. Set DSN environment for PostgreSQL database
2. Set DSN for Sentry in code, remove if needed
3. Create `/cache` directory
4. Compile and run repoproxyd
5. Add repomap item
6. Run repoproxyd
7. Change your repo file to http://hostname:5000/r/reponame
