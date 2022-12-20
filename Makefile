.PHONY: all
all: get agents-operator


.PHONY: get
get: tidy
	go get

tidy:
	go mod tidy

.PHONY: agents-operator
agents-operator: get
	go build -o agents-operator main.go

.PHONY: clean
clean:
	rm agents-operator 
