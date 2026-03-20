FROM python:3.12-alpine
WORKDIR /app
COPY proxy.py .
EXPOSE 8080
CMD ["python", "proxy.py"]
