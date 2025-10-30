package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"golang.org/x/crypto/bcrypt"
)

type MockRepo struct {
	mock.Mock
}

func (m *MockRepo) GetCustomerByEmail(ctx context.Context, email string) (dbread.Customer, error) {
	args := m.Called(ctx, email)
	return args.Get(0).(dbread.Customer), args.Error(1)
}

func (m *MockRepo) GetCustomerByEmailWithPassword(ctx context.Context, email string) (dbread.GetCustomerByEmailWithPasswordRow, error) {
	args := m.Called(ctx, email)
	return args.Get(0).(dbread.GetCustomerByEmailWithPasswordRow), args.Error(1)
}

func (m *MockRepo) GetCustomerByID(ctx context.Context, id string) (dbread.Customer, error) {
	args := m.Called(ctx, id)
	return args.Get(0).(dbread.Customer), args.Error(1)
}

func (m *MockRepo) CreateCustomer(ctx context.Context, arg dbwrite.CreateCustomerParams) (dbwrite.Customer, error) {
	args := m.Called(ctx, arg)
	return args.Get(0).(dbwrite.Customer), args.Error(1)
}

func TestSignUpWithEmail(t *testing.T) {
	tests := []struct {
		name          string
		email         string
		password      string
		mockSetup     func(*MockRepo)
		expectedError error
		expectToken   bool
	}{
		{
			name:     "successful signup",
			email:    "test@example.com",
			password: "password123",
			mockSetup: func(m *MockRepo) {
				m.On("GetCustomerByEmail", mock.Anything, "test@example.com").Return(dbread.Customer{}, errors.New("no rows"))
				customer := dbwrite.Customer{
					ID:           "test-id",
					Email:        "test@example.com",
					PasswordHash: "hashed-password",
				}
				m.On("CreateCustomer", mock.Anything, mock.AnythingOfType("dbwrite.CreateCustomerParams")).Return(customer, nil)
			},
			expectedError: nil,
			expectToken:   true,
		},
		{
			name:     "user already exists",
			email:    "existing@example.com",
			password: "password123",
			mockSetup: func(m *MockRepo) {
				customer := dbread.Customer{
					ID:    "existing-id",
					Email: "existing@example.com",
				}
				m.On("GetCustomerByEmail", mock.Anything, "existing@example.com").Return(customer, nil)
			},
			expectedError: ErrUserAlreadyExists,
			expectToken:   false,
		},
		{
			name:     "customer creation fails",
			email:    "fail@example.com",
			password: "password123",
			mockSetup: func(m *MockRepo) {
				m.On("GetCustomerByEmail", mock.Anything, "fail@example.com").Return(dbread.Customer{}, errors.New("no rows"))
				m.On("CreateCustomer", mock.Anything, mock.AnythingOfType("dbwrite.CreateCustomerParams")).Return(dbwrite.Customer{}, errors.New("creation failed"))
			},
			expectedError: ErrCustomerCreation,
			expectToken:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := new(MockRepo)

			if tt.mockSetup != nil {
				tt.mockSetup(mockRepo)
			}

			service := &Service{
				repo:   mockRepo,
				jwtKey: []byte("test-key"),
			}

			result, err := service.SignUpWithEmail(context.Background(), tt.email, tt.password)

			if tt.expectedError != nil {
				assert.Equal(t, tt.expectedError, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				if tt.expectToken {
					assert.NotNil(t, result)
					assert.NotEmpty(t, result.Token)
				}
			}

			mockRepo.AssertExpectations(t)
		})
	}
}

func TestSignInWithEmail(t *testing.T) {
	passwordHash, _ := bcrypt.GenerateFromPassword([]byte("correctpassword"), bcrypt.DefaultCost)

	tests := []struct {
		name          string
		email         string
		password      string
		mockSetup     func(*MockRepo)
		expectedError error
		expectToken   bool
	}{
		{
			name:     "successful sign in",
			email:    "test@example.com",
			password: "correctpassword",
			mockSetup: func(m *MockRepo) {
				customer := dbread.GetCustomerByEmailWithPasswordRow{
					ID:           "test-id",
					Email:        "test@example.com",
					PasswordHash: string(passwordHash),
				}
				m.On("GetCustomerByEmailWithPassword", mock.Anything, "test@example.com").Return(customer, nil)
			},
			expectedError: nil,
			expectToken:   true,
		},
		{
			name:     "user not found",
			email:    "nonexistent@example.com",
			password: "password123",
			mockSetup: func(m *MockRepo) {
				m.On("GetCustomerByEmailWithPassword", mock.Anything, "nonexistent@example.com").Return(dbread.GetCustomerByEmailWithPasswordRow{}, errors.New("no rows"))
			},
			expectedError: ErrInvalidCredentials,
			expectToken:   false,
		},
		{
			name:     "incorrect password",
			email:    "test@example.com",
			password: "wrongpassword",
			mockSetup: func(m *MockRepo) {
				customer := dbread.GetCustomerByEmailWithPasswordRow{
					ID:           "test-id",
					Email:        "test@example.com",
					PasswordHash: string(passwordHash),
				}
				m.On("GetCustomerByEmailWithPassword", mock.Anything, "test@example.com").Return(customer, nil)
			},
			expectedError: ErrInvalidCredentials,
			expectToken:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := new(MockRepo)
			if tt.mockSetup != nil {
				tt.mockSetup(mockRepo)
			}

			service := &Service{
				repo:   mockRepo,
				jwtKey: []byte("test-key"),
			}

			result, err := service.SignInWithEmail(context.Background(), tt.email, tt.password)

			if tt.expectedError != nil {
				assert.Equal(t, tt.expectedError, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				if tt.expectToken {
					assert.NotNil(t, result)
					assert.NotEmpty(t, result.Token)
				}
			}

			mockRepo.AssertExpectations(t)
		})
	}
}

func TestGenerateJWT(t *testing.T) {
	service := &Service{
		jwtKey: []byte("test-key"),
	}

	tokenString, err := service.generateJWT("test@example.com")
	assert.NoError(t, err)
	assert.NotEmpty(t, tokenString)

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return service.jwtKey, nil
	})
	assert.NoError(t, err)
	assert.True(t, token.Valid)

	claims, ok := token.Claims.(jwt.MapClaims)
	assert.True(t, ok)
	assert.Equal(t, "test@example.com", claims["email"])
	assert.NotNil(t, claims["exp"])
	assert.NotNil(t, claims["iat"])

	exp := int64(claims["exp"].(float64))
	now := time.Now().Unix()
	assert.Greater(t, exp, now)
	assert.Less(t, exp, now+25*3600)
}
